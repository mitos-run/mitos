-- Adds the usage record region dimension (issue #712 phase 0): the placement
-- value the sandbox's tree root was created in, populated best-effort from
-- the sandbox's mitos.run/region label at attribution time. Additive with a
-- '' default so existing rows read back as unattributed rather than
-- requiring a backfill; it carries no billing math change and is not part of
-- the (org_id, sandbox_id, window_start) idempotency key.
ALTER TABLE usage_records ADD COLUMN region TEXT NOT NULL DEFAULT '';
