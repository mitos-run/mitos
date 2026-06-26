-- 0001_init.sql: the durable schema for the SaaS front door (accounts,
-- organizations, memberships, API keys). This is the persistence behind the
-- saas.Store seam; the in-memory MemStore is the behavioral reference and this
-- schema must satisfy the same contract (unique email, sole-owner protection,
-- key lookup by hash on the verify path).
--
-- All timestamps are timestamptz. A zero Go time is stored as NULL so the
-- "never expires" / "live" semantics (ExpiresAt / RevokedAt zero value) round
-- trip cleanly. No raw key value is ever stored; only the salted hash.

CREATE TABLE accounts (
    id              TEXT        PRIMARY KEY,
    email           TEXT        NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ,
    personal_org_id TEXT        NOT NULL DEFAULT '',
    display_name    TEXT        NOT NULL DEFAULT '',
    timezone        TEXT        NOT NULL DEFAULT '',
    locale          TEXT        NOT NULL DEFAULT ''
);

CREATE TABLE orgs (
    id         TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ,
    personal   BOOLEAN     NOT NULL DEFAULT FALSE
);

CREATE TABLE memberships (
    org_id     TEXT        NOT NULL,
    account_id TEXT        NOT NULL,
    role       TEXT        NOT NULL,
    created_at TIMESTAMPTZ,
    PRIMARY KEY (org_id, account_id)
);

-- account_id is the hot lookup for ListMemberships.
CREATE INDEX memberships_account_id_idx ON memberships (account_id);

CREATE TABLE api_keys (
    id         TEXT        PRIMARY KEY,
    org_id     TEXT        NOT NULL,
    name       TEXT        NOT NULL DEFAULT '',
    prefix     TEXT        NOT NULL DEFAULT '',
    hash       TEXT        NOT NULL,
    scopes     TEXT[]      NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

-- hash is the verify path: the key service computes the hash from the presented
-- raw key and looks it up here. Unique so a hash maps to exactly one key.
CREATE UNIQUE INDEX api_keys_hash_idx ON api_keys (hash);

-- org_id is the listing path (ListApiKeys, scoped to one org).
CREATE INDEX api_keys_org_id_idx ON api_keys (org_id);
