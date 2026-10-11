package receipts_test

// These tests talk to a real Postgres (DATABASE_URL, or TEST_DATABASE_URL if
// set), because the guarantees they check are enforced by the database.
// Each test creates its own fresh issue, so they can run against the same
// database as the app without interfering with the seed data.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"sutra/internal/db"
	"sutra/internal/receipts"
)

func setup(t *testing.T) (*pgxpool.Pool, *receipts.Service) {
	t.Helper()
	_ = godotenv.Load("../../.env")
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Fatal("set DATABASE_URL (or TEST_DATABASE_URL) in .env to run these tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, receipts.NewService(pool)
}

// newIssue creates an issue of the given quantity for the first seeded party and SKU.
func newIssue(t *testing.T, pool *pgxpool.Pool, qty int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on, rate_paise)
		VALUES ((SELECT min(id) FROM parties), (SELECT min(id) FROM skus), $1, current_date, 1000)
		RETURNING id`, qty).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Ten receipts of 3 pieces each arrive at the same moment for an issue of 10.
// Only 3 can fit (9 pieces). Without the row lock, several requests would
// each see "enough left" and the total would go past 10.
func TestConcurrentReceiptsNeverOvershoot(t *testing.T) {
	pool, svc := setup(t)
	issueID := newIssue(t, pool, 10)

	const requests, qtyEach = 10, 3
	start := make(chan struct{}) // closed to release all goroutines at once
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, rejected int
	var unexpected []error

	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Create(context.Background(), receipts.Input{
				IssueID:        issueID,
				Qty:            qtyEach,
				ReceivedOn:     "2026-10-10",
				IdempotencyKey: fmt.Sprintf("overshoot-%d-%d", issueID, i),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, receipts.ErrOvershoot):
				rejected++
			default:
				unexpected = append(unexpected, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected errors: %v", unexpected)
	}

	received := countRows(t, pool, `SELECT COALESCE(SUM(qty),0) FROM jobwork_receipts WHERE issue_id = $1`, issueID)
	if received > 10 {
		t.Fatalf("overshoot: received %d of 10 issued", received)
	}
	if ok != 3 || rejected != requests-3 {
		t.Fatalf("want 3 accepted and %d rejected, got %d accepted and %d rejected", requests-3, ok, rejected)
	}
	if received != ok*qtyEach {
		t.Fatalf("received %d does not match %d accepted receipts", received, ok)
	}

	// Every accepted receipt has exactly one voucher (same transaction).
	vouchers := countRows(t, pool, `
		SELECT count(*) FROM vouchers v JOIN jobwork_receipts r
		  ON v.type = 'jobwork_receipt' AND v.source_id = r.id
		WHERE r.issue_id = $1`, issueID)
	if vouchers != ok {
		t.Fatalf("want %d vouchers, got %d", ok, vouchers)
	}
}

// The same Idempotency-Key is sent once, then replayed six more times
// (some at the same moment). Exactly one receipt and one voucher must exist,
// and every replay must return the original receipt and voucher.
func TestIdempotencyKeyReplayCreatesNothing(t *testing.T) {
	pool, svc := setup(t)
	issueID := newIssue(t, pool, 50)
	key := fmt.Sprintf("replay-%d-%d", issueID, time.Now().UnixNano())
	in := receipts.Input{IssueID: issueID, Qty: 5, ReceivedOn: "2026-10-10", IdempotencyKey: key}

	first, err := svc.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Fatal("first request must not be marked as a replay")
	}

	// One sequential replay, then five concurrent ones.
	results := make([]receipts.Result, 6)
	errs := make([]error, 6)
	results[0], errs[0] = svc.Create(context.Background(), in)
	var wg sync.WaitGroup
	for i := 1; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.Create(context.Background(), in)
		}()
	}
	wg.Wait()

	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("replay %d: %v", i, errs[i])
		}
		if !res.Replayed {
			t.Errorf("replay %d not marked as replayed", i)
		}
		if res.Receipt.ID != first.Receipt.ID || res.Voucher.ID != first.Voucher.ID {
			t.Errorf("replay %d returned receipt %d / voucher %d, want %d / %d",
				i, res.Receipt.ID, res.Voucher.ID, first.Receipt.ID, first.Voucher.ID)
		}
	}

	if n := countRows(t, pool, `SELECT count(*) FROM jobwork_receipts WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("want 1 receipt, got %d", n)
	}
	if n := countRows(t, pool, `
		SELECT count(*) FROM vouchers WHERE type = 'jobwork_receipt' AND source_id = $1`, first.Receipt.ID); n != 1 {
		t.Fatalf("want 1 voucher, got %d", n)
	}

	// Reusing the key with a different body is refused and creates nothing.
	changed := in
	changed.Qty = 7
	if _, err := svc.Create(context.Background(), changed); !errors.Is(err, receipts.ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM jobwork_receipts WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("after mismatched replay: want 1 receipt, got %d", n)
	}
}
