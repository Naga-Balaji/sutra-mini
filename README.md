# Sutra Mini

Records job-work receipts and reliably pushes the resulting accounting vouchers
to a slow, failure-prone accounting API.

```
                 POST /jobwork/receipts
 client ─────────────────────────────► api (Go) ──── one transaction ────► Postgres (Neon)
                                                     receipt + voucher        │
                                                     status = pending         │ claim (SKIP LOCKED + lease)
                                                                              ▼
 /sync page (Next.js) ◄── GET /vouchers ── api        worker(s) (Go) ── POST /api/vouchers ──► mock accounting (Go)
                      ── POST /vouchers/{id}/retry                    ◄── 201 / 409 / 500 / hang
```

## Run it from a clean machine

**You need:** Go 1.22+, Node.js 20+, and a free [Neon](https://neon.tech) project.

```bash
git clone <this repo> && cd sutra-mini
cp .env.example .env          # paste your Neon connection string into DATABASE_URL

go run ./cmd/migrate          # creates tables + seed data (safe to run again)
```

Then start each piece in its own terminal, from the repo root:

```bash
go run ./cmd/mock             # fake accounting API on :9090
go run ./cmd/api              # Sutra API on :8080
go run ./cmd/worker           # sync worker (start a second one to test two workers)
cd web && npm install && npm run dev   # open http://localhost:3000/sync
```

Run the tests (they use the database in `.env`, and create their own issues):

```bash
go test ./... -race -count=1
```

## API

| Method | Path | Result |
|---|---|---|
| POST | `/jobwork/receipts` | `201` created, `200` replay (header `Idempotent-Replayed: true`), `409` overshoot, `404` unknown issue, `422` key reused with a different body |
| GET | `/issues/{id}` | `{issued, received, outstanding, ...}` |
| GET | `/vouchers?status=pending\|pushed\|failed` | list (status optional) |
| POST | `/vouchers/{id}/retry` | failed → pending; `409` if not failed |

```bash
curl -i -X POST localhost:8080/jobwork/receipts \
  -H 'Idempotency-Key: demo-1' -H 'Content-Type: application/json' \
  -d '{"issue_id":1,"qty":5,"received_on":"2026-10-10"}'
curl localhost:8080/issues/1
curl 'localhost:8080/vouchers?status=pending'
```

Ten concurrent receipts:

```bash
for i in $(seq 1 10); do
  curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/jobwork/receipts \
    -H "Idempotency-Key: burst-$i" -H 'Content-Type: application/json' \
    -d '{"issue_id":1,"qty":15,"received_on":"2026-10-10"}' &
done; wait
```

## How each guarantee is met

**No overshoot under concurrency.** The receipt transaction first runs
`SELECT ... FROM jobwork_issues WHERE id = $1 FOR UPDATE`. That locks the issue
row, so concurrent receipts for the same issue queue up and run one at a time.
Each one then sums all receipts (including its own) and rolls back if the sum
exceeds the issued quantity. Receipts for *different* issues don't block each other.

**Receipt and voucher in one transaction.** Both `INSERT`s run inside the same
`BEGIN ... COMMIT`. Any error rolls back both.

**Idempotency enforced by Postgres.** `jobwork_receipts.idempotency_key` has a
`UNIQUE` constraint. The insert uses `ON CONFLICT (idempotency_key) DO NOTHING`.
If nothing was inserted, the key was seen before, and we return the original
receipt and voucher. If two requests with the same key arrive at once, Postgres
makes the second wait for the first and then treats it as a replay. Reusing a
key with a *different* body returns `422`. A request that was rejected (for
example, overshoot) stores nothing, so replaying it is evaluated again.

**Never posted twice, even across crashes.** Each voucher gets a `client_ref`
when it is created, and every attempt sends that same value. If a worker dies
after the accounting API recorded the voucher but before our DB was updated,
the next attempt gets `409` with the original `external_ref`, and we mark the
voucher `pushed` with that ref. The accounting side only ever has one copy.

**Two workers at once.** Workers claim vouchers with
`UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)`, setting a lease
(`locked_by`, `locked_until`). Other workers skip rows that are locked or
leased. If a worker crashes, its lease expires (`WORKER_LEASE`, default 15s)
and another worker takes over. Failure updates check `locked_by`, so a worker
that lost its lease cannot overwrite the new owner's result.

**Backoff and failure.** Retryable errors (5xx, timeouts, connection errors)
schedule the next try at `base × 2^(attempt-1)` plus jitter, capped at
`WORKER_BACKOFF_MAX`. After `WORKER_MAX_ATTEMPTS`, or on a non-retryable 4xx, the
voucher becomes `failed` with `last_error`. Retry from the API or the `/sync` page
resets attempts and makes it due now.

**Timeouts and the 5-second hang.** The HTTP client has a timeout
(`WORKER_HTTP_TIMEOUT`, 8s), and each voucher is pushed in its own goroutine
(up to `WORKER_CONCURRENCY`). A hanging call holds only its own slot. The lease
is checked at startup to be longer than the HTTP timeout.

**Money.** Stored as integer paise (`BIGINT`), never as floats.

## Demo scenarios (for the recording)

- **Retry succeeding:** start the mock with `MOCK_FAIL_RATE=1`, the worker with
  `WORKER_MAX_ATTEMPTS=2`, and post a receipt. It ends up `failed` on `/sync`.
  Restart the mock normally (`MOCK_FAIL_RATE=0` for a clean demo) and click **Retry**.
- **Duplicate rejected:** run the mock with `MOCK_HANG_RATE=1 MOCK_FAIL_RATE=0`,
  post a receipt, start a worker, and `kill -9` it within 5s. Restart the worker
  (keep the same mock running). After the lease expires, the worker log shows
  `duplicate reconciled via 409` and the mock log shows `mock: duplicate`.

## Stretch: `POST /ask`

```bash
curl -s -X POST localhost:8080/ask -H 'Content-Type: application/json' \
  -d '{"question":"How much of ANK-001 is still with Meena?"}'
```

- The LLM is called through [OpenRouter](https://openrouter.ai). Set
  `OPENROUTER_API_KEY` in `.env`. The model is `OPENROUTER_MODEL`, default
  `anthropic/claude-haiku-5.5`.
- **Read-only tools only:** `find_skus`, `find_job_workers`, `get_outstanding`.
  The model picks a tool and arguments. It never writes SQL. Each tool runs a
  fixed, parameterised query inside a Postgres `READ ONLY` transaction.
- **Every tool call is logged** (`msg="ask tool call"` in the API output) and
  returned in the response under `tool_calls`, with arguments, result and duration.
- **No key? Stub model.** With `OPENROUTER_API_KEY` empty, a stub picks the SKU
  and job-worker by word matching, calls the same tools, and answers with a
  fixed sentence. The response says `"model": "stub"` and the answer starts
  with `[stub model]`.
- Code: `internal/ask/` (`ask.go` loop + OpenRouter, `tools.go`, `stub.go`).

## Assumptions and limits

- The brief gives no price, so each issue has `rate_paise` (migration 0002), and
  voucher amount = `qty × rate_paise`.
- `Idempotency-Key` is optional. Without it there is no deduplication.
- `POST /vouchers/{id}/retry` only accepts `failed` vouchers, and resets `attempts`.
- The mock keeps its records in memory. Restarting the **mock** forgets past
  `client_ref`s, which a real accounting API would not.
- A replay returns the same receipt and voucher ids. The voucher's status may
  have moved on (for example, to `pushed`) since the first response.
- If you use Neon's pooled (`-pooler`) host and see prepared-statement errors,
  use the direct connection string instead.

## Layout

```
cmd/          migrate, api, worker, mock (one main.go each)
internal/
  receipts/   receipt transaction + the two required tests
  vouchers/   voucher model, list, retry
  syncworker/ claim, push, backoff
  mockacct/   misbehaving accounting API
  api/        HTTP handlers
  ask/        POST /ask: LLM loop, read-only tools, stub
  db/         pool + migration runner
migrations/   0001 given schema, 0002 reliability changes, 0003 seed
web/          Next.js /sync page
```
