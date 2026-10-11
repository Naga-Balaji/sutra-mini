// Package syncworker pushes pending vouchers to the accounting API.
//
// How the guarantees are met:
//
//   - Two workers at once: a voucher is "claimed" with
//     SELECT ... FOR UPDATE SKIP LOCKED and given a lease (locked_by,
//     locked_until). Another worker skips rows that are locked or leased.
//
//   - Crash between the HTTP call and the DB update: the lease simply
//     expires and another (or the restarted) worker claims the voucher again.
//     It re-sends the SAME client_ref, so the accounting API answers 409 with
//     the original external_ref, and we mark it pushed. The voucher is posted
//     once; the second call is only a lookup.
//
//   - Slow calls: every HTTP call has a timeout, and each voucher runs in its
//     own goroutine, so one 5-second hang never holds up the others.
package syncworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/vouchers"
)

type Config struct {
	WorkerID      string
	AccountingURL string        // e.g. http://localhost:9090
	Concurrency   int           // max vouchers in flight at once
	PollInterval  time.Duration // how often to look for due vouchers
	HTTPTimeout   time.Duration // per-call timeout to the accounting API
	Lease         time.Duration // must be longer than HTTPTimeout
	MaxAttempts   int           // after this many, the voucher is marked failed
	BackoffBase   time.Duration // first retry delay; doubles each attempt
	BackoffMax    time.Duration // cap on the retry delay
}

type Worker struct {
	cfg    Config
	pool   *pgxpool.Pool
	client *http.Client
	log    *slog.Logger
}

func New(cfg Config, pool *pgxpool.Pool, log *slog.Logger) (*Worker, error) {
	if cfg.Lease <= cfg.HTTPTimeout {
		return nil, fmt.Errorf("lease (%s) must be longer than the HTTP timeout (%s)", cfg.Lease, cfg.HTTPTimeout)
	}
	return &Worker{
		cfg:    cfg,
		pool:   pool,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		log:    log.With("worker", cfg.WorkerID),
	}, nil
}

// Run loops until ctx is cancelled (Ctrl+C), then waits for in-flight
// vouchers to finish recording their outcome.
func (w *Worker) Run(ctx context.Context) error {
	slots := make(chan struct{}, w.cfg.Concurrency) // a counting semaphore
	var wg sync.WaitGroup
	defer wg.Wait()

	w.log.Info("worker started", "concurrency", w.cfg.Concurrency, "http_timeout", w.cfg.HTTPTimeout, "lease", w.cfg.Lease)
	for {
		if free := cap(slots) - len(slots); free > 0 {
			claimed, err := w.claim(ctx, free)
			if err != nil && ctx.Err() == nil {
				w.log.Error("claim failed", "err", err)
			}
			for _, v := range claimed {
				slots <- struct{}{}
				wg.Add(1)
				go func(v vouchers.Voucher) {
					defer func() { <-slots; wg.Done() }()
					w.push(ctx, v)
				}(v)
			}
		}

		select {
		case <-ctx.Done():
			w.log.Info("worker stopping; waiting for in-flight vouchers")
			return nil
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// claim takes up to limit due vouchers and leases them to this worker.
// FOR UPDATE SKIP LOCKED: if another worker is claiming a row right now,
// skip it instead of waiting. The lease check skips rows another worker
// already owns. attempts is counted here, so a crash also counts as a try.
func (w *Worker) claim(ctx context.Context, limit int) ([]vouchers.Voucher, error) {
	rows, err := w.pool.Query(ctx, `
		UPDATE vouchers
		SET locked_by = $1,
		    locked_until = now() + $2::float8 * interval '1 millisecond',
		    attempts = attempts + 1,
		    updated_at = now()
		WHERE id IN (
			SELECT id FROM vouchers
			WHERE status = 'pending'
			  AND next_attempt_at <= now()
			  AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_attempt_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+vouchers.Columns,
		w.cfg.WorkerID, float64(w.cfg.Lease.Milliseconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []vouchers.Voucher
	for rows.Next() {
		v, err := vouchers.Scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

// outcome describes what the accounting API told us.
type outcome struct {
	externalRef string // set on success (201) or duplicate (409)
	duplicate   bool
	err         error
	retryable   bool
}

func (w *Worker) push(ctx context.Context, v vouchers.Voucher) {
	log := w.log.With("voucher", v.ID, "client_ref", v.ClientRef, "attempt", v.Attempts)
	out := w.send(ctx, v)

	// Record the result even if we are shutting down: WithoutCancel keeps the
	// values of ctx but ignores its cancellation, and we add our own timeout.
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if out.err == nil {
		if out.duplicate {
			log.Info("duplicate reconciled via 409", "external_ref", out.externalRef)
		} else {
			log.Info("pushed", "external_ref", out.externalRef)
		}
		if err := w.markPushed(dbCtx, v.ID, out.externalRef); err != nil {
			log.Error("could not record push; lease will expire and the 409 path will reconcile", "err", err)
		}
		return
	}

	final := !out.retryable || v.Attempts >= w.cfg.MaxAttempts
	delay := w.backoff(v.Attempts)
	if final {
		log.Warn("giving up", "err", out.err)
	} else {
		log.Warn("attempt failed; will retry", "err", out.err, "retry_in", delay)
	}
	if err := w.markFailedAttempt(dbCtx, v.ID, out.err.Error(), final, delay); err != nil {
		log.Error("could not record failure", "err", err)
	}
}

// send makes one HTTP call and classifies the response.
func (w *Worker) send(ctx context.Context, v vouchers.Voucher) outcome {
	body, _ := json.Marshal(map[string]any{
		"client_ref":   v.ClientRef,
		"type":         v.Type,
		"source_id":    v.SourceID,
		"amount_paise": v.AmountPaise,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.AccountingURL+"/api/vouchers", bytes.NewReader(body))
	if err != nil {
		return outcome{err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		// Timeouts and connection errors: we don't know whether the API
		// recorded it, so retry. The client_ref makes the retry safe.
		return outcome{err: fmt.Errorf("request failed: %w", err), retryable: true}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var parsed struct {
		ExternalRef string `json:"external_ref"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &parsed)

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		if parsed.ExternalRef == "" {
			return outcome{err: errors.New("success response without external_ref"), retryable: true}
		}
		return outcome{externalRef: parsed.ExternalRef}
	case resp.StatusCode == http.StatusConflict:
		if parsed.ExternalRef == "" {
			return outcome{err: errors.New("409 without external_ref"), retryable: false}
		}
		return outcome{externalRef: parsed.ExternalRef, duplicate: true}
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return outcome{err: fmt.Errorf("accounting API returned %d: %s", resp.StatusCode, parsed.Error), retryable: true}
	default:
		// Other 4xx: our request is wrong; retrying won't help.
		return outcome{err: fmt.Errorf("accounting API rejected voucher (%d): %s", resp.StatusCode, parsed.Error), retryable: false}
	}
}

// backoff: base * 2^(attempt-1), capped, plus up to 20% random jitter so
// many vouchers failing together don't all retry at the same instant.
func (w *Worker) backoff(attempt int) time.Duration {
	d := w.cfg.BackoffBase
	for i := 1; i < attempt && d < w.cfg.BackoffMax; i++ {
		d *= 2
	}
	d = min(d, w.cfg.BackoffMax)
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}

// markPushed does not check the lease: the accounting API is the source of
// truth, and it has an external_ref for this voucher, so it IS pushed.
func (w *Worker) markPushed(ctx context.Context, id int64, externalRef string) error {
	_, err := w.pool.Exec(ctx, `
		UPDATE vouchers
		SET status = 'pushed', external_ref = $2, last_error = NULL,
		    locked_by = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1`, id, externalRef)
	return err
}

// markFailedAttempt only applies if we still hold the lease. If our lease
// expired and another worker took over, its result wins and ours is dropped.
func (w *Worker) markFailedAttempt(ctx context.Context, id int64, reason string, final bool, delay time.Duration) error {
	status := vouchers.StatusPending
	if final {
		status = vouchers.StatusFailed
	}
	tag, err := w.pool.Exec(ctx, `
		UPDATE vouchers
		SET status = $3, last_error = $4,
		    next_attempt_at = now() + $5::float8 * interval '1 millisecond',
		    locked_by = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND locked_by = $2 AND status = 'pending'`,
		id, w.cfg.WorkerID, status, reason, float64(delay.Milliseconds()))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		w.log.Warn("lease lost before recording failure; leaving voucher to its new owner", "voucher", id)
	}
	return nil
}
