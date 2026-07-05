-- Extends the durable audit trail (migration 0009) with the human-readable
-- actor and target fields the console audit view now renders instead of bare
-- ids (issue: durable, human-readable audit log). Rather than standing up a
-- second table, this ALTERs the existing audit_log table in place: it already
-- carries the org's live audit history, and creating a parallel table would
-- silently orphan that history on every hosted deployment already running
-- migration 0009.
--
-- All four columns default to '' so existing rows (written before ActorName /
-- ActorType / TargetType / TargetName existed on console.AuditEvent) read back
-- as the zero value rather than NULL, matching the Go struct's zero value and
-- keeping PgAuditLog.List's Scan simple (no sql.NullString).
ALTER TABLE audit_log ADD COLUMN actor_name  TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN actor_type  TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN target_type TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN target_name TEXT NOT NULL DEFAULT '';
