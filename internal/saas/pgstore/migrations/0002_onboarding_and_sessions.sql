-- Durable onboarding and session state, so a real signup (its $5 credit, its
-- verify link, its login) survives a console restart. Mirrors the in-memory
-- MemCreditLedger, MemPendingStore, and SessionStore.

CREATE TABLE sessions (
    id         TEXT        PRIMARY KEY,
    token_hash TEXT        NOT NULL UNIQUE,
    account_id TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    label      TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

CREATE TABLE credit_ledger (
    id          BIGSERIAL   PRIMARY KEY,
    org_id      TEXT        NOT NULL,
    kind        TEXT        NOT NULL,
    amount      BIGINT      NOT NULL,
    idem_key    TEXT        NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL,
    note        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX credit_ledger_org_id_idx ON credit_ledger (org_id);
-- Idempotency: a non-empty key is unique per org. Empty keys are non-idempotent
-- and may repeat, so they are excluded from the unique constraint.
CREATE UNIQUE INDEX credit_ledger_org_key_idx ON credit_ledger (org_id, idem_key) WHERE idem_key <> '';

CREATE TABLE pending_signups (
    id         TEXT        PRIMARY KEY,
    email      TEXT        NOT NULL,
    token_hash TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    verified   BOOLEAN     NOT NULL DEFAULT FALSE,
    account_id TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE waitlist_entries (
    id         BIGSERIAL   PRIMARY KEY,
    email      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
