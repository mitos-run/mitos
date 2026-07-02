-- Durable, append-only org audit trail (issue #616). The console previously
-- wired only the in-memory MemAuditLog, so the org audit trail (key creation
-- and revocation, member changes, billing actions) was wiped on every console
-- restart. One row per console.AuditEvent. Rows are NEVER updated; the only
-- permitted delete is retention pruning by age (the per-org retention policy
-- the console stores; its enforcement sweep is the controller GC follow-up,
-- issue #163). detail is a non-secret, human-legible summary and never carries
-- a key value or any secret.
CREATE TABLE audit_log (
    id         BIGSERIAL   PRIMARY KEY,
    org_id     TEXT        NOT NULL,
    actor      TEXT        NOT NULL DEFAULT '',
    action     TEXT        NOT NULL DEFAULT '',
    target     TEXT        NOT NULL DEFAULT '',
    detail     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);

-- The console reads one org's trail most recent first; this index serves both
-- the org-scoped filter and the reverse-chronological order.
CREATE INDEX audit_log_org_created_at ON audit_log (org_id, created_at DESC);
