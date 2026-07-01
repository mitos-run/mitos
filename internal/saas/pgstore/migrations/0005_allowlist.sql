-- Signup allowlist: an email in this table may provision (Workstream A). Presence
-- means approved; there is no status enum. The email is the canonical address
-- (lowercased). note is a non-secret operator memo. Auto-allow domains are applied
-- in the application layer, not here, so this table holds only per-email grants.
CREATE TABLE allowlist (
    email      TEXT        PRIMARY KEY,
    note       TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
