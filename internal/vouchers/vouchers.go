// Package vouchers holds the voucher model and the queries the API uses.
package vouchers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/db"
)

const (
	StatusPending = "pending"
	StatusPushed  = "pushed"
	StatusFailed  = "failed"

	TypeJobworkReceipt = "jobwork_receipt"
)

var (
	ErrNotFound      = errors.New("voucher not found")
	ErrNotRetryable  = errors.New("only failed vouchers can be retried")
	ErrInvalidStatus = errors.New("status must be pending, pushed or failed")
)

type Voucher struct {
	ID            int64     `json:"id"`
	Type          string    `json:"type"`
	SourceID      int64     `json:"source_id"`
	AmountPaise   int64     `json:"amount_paise"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     *string   `json:"last_error"` // pointer: NULL in the DB becomes null in JSON
	ClientRef     string    `json:"client_ref"`
	ExternalRef   *string   `json:"external_ref"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Columns lists the fields Scan expects, in order. Every query that returns
// a voucher uses this so the two never drift apart.
const Columns = `id, type, source_id, amount_paise, status, attempts, last_error,
	client_ref, external_ref, next_attempt_at, created_at, updated_at`

func Scan(row pgx.Row) (Voucher, error) {
	var v Voucher
	err := row.Scan(&v.ID, &v.Type, &v.SourceID, &v.AmountPaise, &v.Status, &v.Attempts,
		&v.LastError, &v.ClientRef, &v.ExternalRef, &v.NextAttemptAt, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

// List returns vouchers, optionally filtered by status ("" means all).
func List(ctx context.Context, q db.Querier, status string) ([]Voucher, error) {
	if status != "" && status != StatusPending && status != StatusPushed && status != StatusFailed {
		return nil, ErrInvalidStatus
	}
	rows, err := q.Query(ctx, `
		SELECT `+Columns+` FROM vouchers
		WHERE ($1 = '' OR status = $1)
		ORDER BY id DESC
		LIMIT 500`, status)
	if err != nil {
		return nil, fmt.Errorf("list vouchers: %w", err)
	}
	defer rows.Close()

	list := []Voucher{} // empty slice, so JSON shows [] rather than null
	for rows.Next() {
		v, err := Scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan voucher: %w", err)
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

// Retry moves a failed voucher back to pending so the worker picks it up
// immediately. Attempts reset to 0 so it gets a full set of retries again.
// The client_ref is kept, so if an earlier attempt actually reached the
// accounting API, the retry is reconciled through the 409 path.
func Retry(ctx context.Context, pool *pgxpool.Pool, id int64) (Voucher, error) {
	v, err := Scan(pool.QueryRow(ctx, `
		UPDATE vouchers
		SET status = 'pending', attempts = 0, next_attempt_at = now(),
		    locked_by = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'failed'
		RETURNING `+Columns, id))
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Voucher{}, fmt.Errorf("retry voucher: %w", err)
	}

	// Nothing updated: either it doesn't exist, or it isn't failed.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vouchers WHERE id = $1)`, id).Scan(&exists); err != nil {
		return Voucher{}, fmt.Errorf("check voucher: %w", err)
	}
	if !exists {
		return Voucher{}, ErrNotFound
	}
	return Voucher{}, ErrNotRetryable
}
