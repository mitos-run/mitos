-- Rewrite org ids that begin or end with '-' or '_' to a label-safe form by
-- replacing the offending edge character with '0' (#593). Org ids minted by
-- the old base64url randomID could carry those edge characters, and the
-- gateway stamps every Sandbox with the org id as a Kubernetes label VALUE,
-- which must begin and end alphanumeric: an affected org could never create a
-- sandbox. New ids are lowercase base32 and always safe; this migration heals
-- the ids minted before the fix.
--
-- The same transform is applied to every table that carries an org id; these
-- tables have no FK constraints between them, so order does not matter. A
-- collision between a rewritten id and an existing one would violate the orgs
-- primary key and abort the migration for the operator to resolve; with
-- 16-character ids the probability is negligible.

UPDATE orgs SET id = regexp_replace(id, '^[-_]|[-_]$', '0', 'g')
	WHERE id ~ '^[-_]|[-_]$';
UPDATE memberships SET org_id = regexp_replace(org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE org_id ~ '^[-_]|[-_]$';
UPDATE api_keys SET org_id = regexp_replace(org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE org_id ~ '^[-_]|[-_]$';
UPDATE credit_ledger SET org_id = regexp_replace(org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE org_id ~ '^[-_]|[-_]$';
UPDATE usage_records SET org_id = regexp_replace(org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE org_id ~ '^[-_]|[-_]$';
UPDATE spend_caps SET org_id = regexp_replace(org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE org_id ~ '^[-_]|[-_]$';
UPDATE accounts SET personal_org_id = regexp_replace(personal_org_id, '^[-_]|[-_]$', '0', 'g')
	WHERE personal_org_id ~ '^[-_]|[-_]$';
