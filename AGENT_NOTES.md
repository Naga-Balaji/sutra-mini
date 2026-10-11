# AGENT_NOTES

> Draft written during the build session. **Edit this in your own words before
> submitting.** The brief asks for candid notes, and the "one thing I caught"
> section must be yours.

## Tools used

- Claude Code (model: Claude Opus 5.5) in the terminal, with the VibeWise
  learning plugin. Claude wrote the code. I asked it to explain each piece at a
  beginner level, because I am new to Go.
- Local Postgres 18 (a throwaway database) for fast verification, and Neon for
  the real setup.
- Stretch `POST /ask`: runs through OpenRouter. I tested with the free model
  `apodex/apodex-1.1-mini:free`; the code default is `anthropic/claude-haiku-5.5`.
  On real Neon data it answered 4/4 test questions correctly, and it refused
  "Delete all vouchers" because it has no write tool.
- Recording: Claude wrote a script that runs the demo live against Neon in a
  split terminal (API, mock and worker logs + commands). The terminal was
  recorded with asciinema, and the narration uses the macOS text-to-speech
  voice "Aman".

## How I worked with it

1. Gave it the brief PDF and asked it to build the whole thing.
2. It wrote migrations → receipts transaction → API → mock → worker → tests → page.
3. Verification that was actually run:
   - `go build`, `go vet`, `go test -race`: passing.
   - **Mutation checks:** removing `FOR UPDATE` made the overshoot test fail
     (15 received of 10 issued). Dropping the unique constraint plus
     `ON CONFLICT` made the idempotency test fail (replays created new receipts).
     Both pass again once restored.
   - End to end with two workers: 10 concurrent receipts of 5 against an issue
     of 25 gave 5 × 201 and 5 × 409. A replay gave 200 with `Idempotent-Replayed: true`.
   - Worker `kill -9` during the 5s hang, then restart: the mock recorded the
     voucher once, and the restarted worker got 409 with the original
     `external_ref` and marked it pushed.
   - Retry: a voucher failed against an always-500 mock, Retry was pressed
     through the Next.js proxy, and it was pushed.

## What the agent got wrong

- Its first end-to-end shell script used a bare `wait`, which also waited on
  the long-running servers, so the script hung until it timed out.
- Its "kill during hang" script reused an issue that was already fully
  received, so the test receipt was (correctly) rejected as an overshoot and the
  scenario never ran. It needed a fresh issue.
- (Add anything else you noticed.)

## One thing I caught

**The tests were writing into the same database as the app, and it showed up in
the answers.**

When I tested the `/ask` stretch feature against my Neon data, the answer for
"How much of ANK-001 is still with Meena?" said **160 issued**. The seed data
only issues **100** pieces of ANK-001 to Meena (`GET /issues/2` shows 100).

The extra 60 came from the tests. Each test run creates its own issues (10 and
50 pieces) for the *first* seeded SKU and job-worker, in the same database that
`DATABASE_URL` points to. The agent wrote tests that were correct for proving
the guarantees, but it didn't consider that every `go test` run would
quietly change the demo data. Anyone running the tests before a demo would see
numbers that don't match the seed.

The guarantees themselves weren't affected. The fix is to give tests their
own database: the code already reads `TEST_DATABASE_URL`, so it only needs to
point at a separate Neon branch. I haven't switched yet, so my demo database
still contains the test-created issues.

## Decisions worth knowing for the live change

- Overshoot: row lock on the issue (`FOR UPDATE`) + `SUM` check, in `internal/receipts/receipts.go`.
- Idempotency: `UNIQUE (idempotency_key)` + `ON CONFLICT DO NOTHING`.
- Never twice: a stable `client_ref` per voucher + the 409 reconcile path in `internal/syncworker/worker.go`.
- Two workers: `FOR UPDATE SKIP LOCKED` + lease columns (`locked_by`, `locked_until`).
- Assumption: `rate_paise` on issues, because the brief gives no price.
