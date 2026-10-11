package ask

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The model can only call these fixed, parameterised queries. It never writes
// SQL itself, and every query runs in a READ ONLY transaction, so even a bug
// here cannot change data.

// toolSpecs is what we send to the model so it knows which tools exist.
var toolSpecs = []map[string]any{
	tool("find_skus",
		"Search SKUs (products) by code or name. Empty query lists all SKUs.",
		map[string]any{"query": map[string]any{"type": "string", "description": "Part of the SKU code or name, e.g. 'ANK-001' or 'anarkali'."}}),
	tool("find_job_workers",
		"Search job-workers (embroidery/stitching units) by name. Empty query lists all.",
		map[string]any{"query": map[string]any{"type": "string", "description": "Part of the job-worker's name, e.g. 'meena'."}}),
	tool("get_outstanding",
		"For one SKU and one job-worker, list every job-work issue with issued, received and outstanding (still with the job-worker) quantities, plus totals.",
		map[string]any{
			"sku_id":   map[string]any{"type": "integer", "description": "id from find_skus"},
			"party_id": map[string]any{"type": "integer", "description": "id from find_job_workers"},
		}, "sku_id", "party_id"),
}

func tool(name, description string, props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters":  map[string]any{"type": "object", "properties": props, "required": required},
		},
	}
}

type tools struct {
	pool *pgxpool.Pool
}

// run executes one tool call and returns its result as JSON text.
func (t tools) run(ctx context.Context, name string, argsJSON string) (string, error) {
	var args struct {
		Query   string `json:"query"`
		SKUID   int64  `json:"sku_id"`
		PartyID int64  `json:"party_id"`
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
	}

	var result any
	err := pgx.BeginTxFunc(ctx, t.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		switch name {
		case "find_skus":
			result, err = findSKUs(ctx, tx, args.Query)
		case "find_job_workers":
			result, err = findParties(ctx, tx, args.Query)
		case "get_outstanding":
			result, err = getOutstanding(ctx, tx, args.SKUID, args.PartyID)
		default:
			err = fmt.Errorf("unknown tool %q", name)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	return string(out), err
}

type sku struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

func findSKUs(ctx context.Context, tx pgx.Tx, q string) ([]sku, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, code, name FROM skus
		WHERE $1 = '' OR code ILIKE '%' || $1 || '%' OR name ILIKE '%' || $1 || '%'
		ORDER BY code LIMIT 20`, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[sku])
}

type party struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func findParties(ctx context.Context, tx pgx.Tx, q string) ([]party, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, name FROM parties
		WHERE $1 = '' OR name ILIKE '%' || $1 || '%'
		ORDER BY name LIMIT 20`, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[party])
}

type issueLine struct {
	IssueID     int64  `json:"issue_id"`
	IssuedOn    string `json:"issued_on"`
	Issued      int    `json:"issued"`
	Received    int    `json:"received"`
	Outstanding int    `json:"outstanding"`
}

type outstanding struct {
	SKU              string      `json:"sku"`
	JobWorker        string      `json:"job_worker"`
	Issues           []issueLine `json:"issues"`
	TotalIssued      int         `json:"total_issued"`
	TotalReceived    int         `json:"total_received"`
	TotalOutstanding int         `json:"total_outstanding"`
}

func getOutstanding(ctx context.Context, tx pgx.Tx, skuID, partyID int64) (outstanding, error) {
	var out outstanding
	err := tx.QueryRow(ctx, `
		SELECT (SELECT code || ' ' || name FROM skus WHERE id = $1),
		       (SELECT name FROM parties WHERE id = $2)`, skuID, partyID,
	).Scan(&out.SKU, &out.JobWorker)
	if err != nil {
		return out, err
	}
	if out.SKU == "" || out.JobWorker == "" {
		return out, fmt.Errorf("unknown sku_id %d or party_id %d", skuID, partyID)
	}

	rows, err := tx.Query(ctx, `
		SELECT i.id, i.issued_on::text, i.qty, COALESCE(SUM(r.qty), 0)::int, 0
		FROM jobwork_issues i
		LEFT JOIN jobwork_receipts r ON r.issue_id = i.id
		WHERE i.sku_id = $1 AND i.party_id = $2
		GROUP BY i.id ORDER BY i.issued_on, i.id`, skuID, partyID)
	if err != nil {
		return out, err
	}
	out.Issues, err = pgx.CollectRows(rows, pgx.RowToStructByPos[issueLine])
	if err != nil {
		return out, err
	}
	for i := range out.Issues {
		l := &out.Issues[i]
		l.Outstanding = l.Issued - l.Received
		out.TotalIssued += l.Issued
		out.TotalReceived += l.Received
		out.TotalOutstanding += l.Outstanding
	}
	if out.Issues == nil {
		out.Issues = []issueLine{}
	}
	return out, nil
}
