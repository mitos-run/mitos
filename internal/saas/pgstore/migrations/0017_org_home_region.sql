-- Adds the org home region (issue #712 phase 0 placement registry): the
-- data-residency anchor stamped at org creation time from the deployment's
-- placement.Registry default. Additive, nullable-free with a DEFAULT so
-- existing rows read back as '', meaning "the deployment's registry
-- default" rather than a stored region name; no backfill is required.
ALTER TABLE orgs ADD COLUMN home_region TEXT NOT NULL DEFAULT '';
