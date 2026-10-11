-- The schema exactly as given in the brief, with types and basic constraints.

CREATE TABLE skus (
    id        BIGSERIAL PRIMARY KEY,
    code      TEXT NOT NULL UNIQUE,
    name      TEXT NOT NULL,
    image_url TEXT NOT NULL
);

-- Job-workers (embroidery / stitching units).
CREATE TABLE parties (
    id    BIGSERIAL PRIMARY KEY,
    name  TEXT NOT NULL,
    phone TEXT NOT NULL
);

CREATE TABLE jobwork_issues (
    id        BIGSERIAL PRIMARY KEY,
    party_id  BIGINT  NOT NULL REFERENCES parties(id),
    sku_id    BIGINT  NOT NULL REFERENCES skus(id),
    qty       INTEGER NOT NULL CHECK (qty > 0),
    issued_on DATE    NOT NULL
);

CREATE TABLE jobwork_receipts (
    id              BIGSERIAL PRIMARY KEY,
    issue_id        BIGINT  NOT NULL REFERENCES jobwork_issues(id),
    qty             INTEGER NOT NULL CHECK (qty > 0),
    received_on     DATE    NOT NULL,
    idempotency_key TEXT
);

-- Money is stored as whole paise in a BIGINT: never a float.
CREATE TABLE vouchers (
    id           BIGSERIAL PRIMARY KEY,
    type         TEXT    NOT NULL,
    source_id    BIGINT  NOT NULL,
    amount_paise BIGINT  NOT NULL CHECK (amount_paise >= 0),
    status       TEXT    NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'pushed', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    client_ref   TEXT,
    external_ref TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
