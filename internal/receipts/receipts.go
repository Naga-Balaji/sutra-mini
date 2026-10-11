// Package receipts records goods coming back from job-workers.
package receipts

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/vouchers"
)

var (
	ErrIssueNotFound = errors.New("issue not found")
	ErrOvershoot     = errors.New("receipt would exceed the issued quantity")
	ErrKeyMismatch   = errors.New("idempotency key was already used with a different request")
)

type Input struct {
	IssueID        int64
	Qty            int
	ReceivedOn     string // YYYY-MM-DD, validated by the caller
	IdempotencyKey string // "" means no key
}

type Receipt struct {
	ID             int64   `json:"id"`
	IssueID        int64   `json:"issue_id"`
	Qty            int     `json:"qty"`
	ReceivedOn     string  `json:"received_on"`
	IdempotencyKey *string `json:"idempotency_key"`
}

type Result struct {
	Receipt  Receipt          `json:"receipt"`
	Voucher  vouchers.Voucher `json:"voucher"`
	Replayed bool             `json:"replayed"` // true when this key was seen before
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Create records a receipt and its voucher in ONE transaction.
//
// Step by step:
//  1. Lock the issue row (SELECT ... FOR UPDATE). Any other request for the
//     same issue now waits here until we commit or roll back. This is what
//     makes "no overshoot" hold under concurrency.
//  2. Insert the receipt. ON CONFLICT on the unique idempotency_key means a
//     replayed key inserts nothing; we then return the original result.
//  3. Sum all receipts for the issue (including ours). If the sum is more
//     than issued, roll back: nothing is saved.
//  4. Insert the voucher in the same transaction, then commit.
func (s *Service) Create(ctx context.Context, in Input) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // undoes everything unless we reach Commit

	// 1. Lock the issue.
	var issuedQty int
	var ratePaise int64
	err = tx.QueryRow(ctx,
		`SELECT qty, rate_paise FROM jobwork_issues WHERE id = $1 FOR UPDATE`, in.IssueID,
	).Scan(&issuedQty, &ratePaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrIssueNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("lock issue: %w", err)
	}

	// 2. Insert the receipt, unless this idempotency key already exists.
	var key *string
	if in.IdempotencyKey != "" {
		key = &in.IdempotencyKey
	}
	var r Receipt
	err = tx.QueryRow(ctx, `
		INSERT INTO jobwork_receipts (issue_id, qty, received_on, idempotency_key)
		VALUES ($1, $2, $3::date, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, issue_id, qty, received_on::text, idempotency_key`,
		in.IssueID, in.Qty, in.ReceivedOn, key,
	).Scan(&r.ID, &r.IssueID, &r.Qty, &r.ReceivedOn, &r.IdempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		// The key was used before. Release our lock and return the original.
		_ = tx.Rollback(ctx)
		return s.replay(ctx, in)
	}
	if err != nil {
		return Result{}, fmt.Errorf("insert receipt: %w", err)
	}

	// 3. Check the total received, now including this receipt.
	var totalReceived int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(qty), 0) FROM jobwork_receipts WHERE issue_id = $1`, in.IssueID,
	).Scan(&totalReceived)
	if err != nil {
		return Result{}, fmt.Errorf("sum receipts: %w", err)
	}
	if totalReceived > issuedQty {
		alreadyReceived := totalReceived - in.Qty
		return Result{}, fmt.Errorf("%w: issued %d, already received %d, outstanding %d, requested %d",
			ErrOvershoot, issuedQty, alreadyReceived, issuedQty-alreadyReceived, in.Qty)
	}

	// 4. Create the voucher in the same transaction.
	amount := int64(in.Qty) * ratePaise
	v, err := vouchers.Scan(tx.QueryRow(ctx, `
		INSERT INTO vouchers (type, source_id, amount_paise)
		VALUES ($1, $2, $3)
		RETURNING `+vouchers.Columns,
		vouchers.TypeJobworkReceipt, r.ID, amount))
	if err != nil {
		return Result{}, fmt.Errorf("insert voucher: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	return Result{Receipt: r, Voucher: v}, nil
}

// replay loads the receipt and voucher created by an earlier request with the
// same key. If the earlier request had a different body, that is a client
// bug, and we refuse rather than silently returning someone else's result.
func (s *Service) replay(ctx context.Context, in Input) (Result, error) {
	var r Receipt
	err := s.pool.QueryRow(ctx, `
		SELECT id, issue_id, qty, received_on::text, idempotency_key
		FROM jobwork_receipts WHERE idempotency_key = $1`, in.IdempotencyKey,
	).Scan(&r.ID, &r.IssueID, &r.Qty, &r.ReceivedOn, &r.IdempotencyKey)
	if err != nil {
		return Result{}, fmt.Errorf("load replayed receipt: %w", err)
	}
	if r.IssueID != in.IssueID || r.Qty != in.Qty || r.ReceivedOn != in.ReceivedOn {
		return Result{}, ErrKeyMismatch
	}

	v, err := vouchers.Scan(s.pool.QueryRow(ctx, `
		SELECT `+vouchers.Columns+` FROM vouchers WHERE type = $1 AND source_id = $2`,
		vouchers.TypeJobworkReceipt, r.ID))
	if err != nil {
		return Result{}, fmt.Errorf("load replayed voucher: %w", err)
	}
	return Result{Receipt: r, Voucher: v, Replayed: true}, nil
}

type IssueSummary struct {
	ID          int64  `json:"id"`
	PartyName   string `json:"party_name"`
	SKUCode     string `json:"sku_code"`
	Issued      int    `json:"issued"`
	Received    int    `json:"received"`
	Outstanding int    `json:"outstanding"`
}

// Summary returns issued / received / outstanding for one issue.
func (s *Service) Summary(ctx context.Context, issueID int64) (IssueSummary, error) {
	var sum IssueSummary
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, p.name, k.code, i.qty, COALESCE(SUM(r.qty), 0)
		FROM jobwork_issues i
		JOIN parties p ON p.id = i.party_id
		JOIN skus k    ON k.id = i.sku_id
		LEFT JOIN jobwork_receipts r ON r.issue_id = i.id
		WHERE i.id = $1
		GROUP BY i.id, p.name, k.code`, issueID,
	).Scan(&sum.ID, &sum.PartyName, &sum.SKUCode, &sum.Issued, &sum.Received)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssueSummary{}, ErrIssueNotFound
	}
	if err != nil {
		return IssueSummary{}, fmt.Errorf("issue summary: %w", err)
	}
	sum.Outstanding = sum.Issued - sum.Received
	return sum, nil
}
