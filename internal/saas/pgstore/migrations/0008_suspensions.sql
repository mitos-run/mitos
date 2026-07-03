-- Durable org suspensions so the abuse/billing kill-switch survives a gateway
-- restart and is shared across gateway replicas (issue #615: the in-memory
-- store made the kill-switch advisory with replicas > 1). One row per
-- currently-suspended org; lifting a suspension DELETES the row, so the table
-- holds only live suspensions. The reason and note are non-secret audit text
-- (never a key or token); suspended_at is the FIRST suspension instant and is
-- preserved on a re-suspend (the store updates reason/note/manual_hold only).
CREATE TABLE suspensions (
    org_id       TEXT        PRIMARY KEY,
    reason       TEXT        NOT NULL,
    note         TEXT        NOT NULL DEFAULT '',
    suspended_at TIMESTAMPTZ NOT NULL,
    manual_hold  BOOLEAN     NOT NULL DEFAULT FALSE
);
