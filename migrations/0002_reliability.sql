-- Changes on top of the given schema. Each one exists to back a guarantee
-- with a database rule instead of application memory.

-- Idempotency: one receipt per key, enforced by Postgres.
-- (NULL keys are allowed and never conflict, so the header stays optional.)
ALTER TABLE jobwork_receipts
    ADD CONSTRAINT jobwork_receipts_idempotency_key_key UNIQUE (idempotency_key);

CREATE INDEX jobwork_receipts_issue_id_idx ON jobwork_receipts (issue_id);

-- The brief gives no price, but a voucher needs an amount.
-- Assumption: each issue carries a job-work rate per piece, in paise.
ALTER TABLE jobwork_issues
    ADD COLUMN rate_paise BIGINT NOT NULL DEFAULT 0 CHECK (rate_paise >= 0);

-- client_ref is the id we send to the accounting API. It is generated once,
-- when the voucher is created, and reused on every retry. That is what lets
-- the accounting side recognise a duplicate (HTTP 409).
ALTER TABLE vouchers
    ALTER COLUMN client_ref SET DEFAULT ('sutra-' || gen_random_uuid()::text),
    ALTER COLUMN client_ref SET NOT NULL,
    ADD CONSTRAINT vouchers_client_ref_key UNIQUE (client_ref);

-- At most one voucher per source document.
ALTER TABLE vouchers
    ADD CONSTRAINT vouchers_type_source_id_key UNIQUE (type, source_id);

-- Sync worker bookkeeping:
--   next_attempt_at  when the voucher may be tried next (backoff)
--   locked_by        which worker currently owns it
--   locked_until     lease expiry; after this, another worker may take over
ALTER TABLE vouchers
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN locked_by       TEXT,
    ADD COLUMN locked_until    TIMESTAMPTZ;

CREATE INDEX vouchers_pending_due_idx ON vouchers (next_attempt_at) WHERE status = 'pending';
CREATE INDEX vouchers_status_idx ON vouchers (status);
