-- Durable, billable usage records so per-org metered consumption survives a
-- controller restart (issue #211). Mirrors the in-memory usage.MemUsageStore.
-- The primary key is the integrator's idempotency key (org_id, sandbox_id,
-- window_start): re-collecting an overlapping or duplicate scrape upserts the
-- same key with the recomputed value, so a node loss, a controller restart, or a
-- late scrape can never double-bill a window.
--
-- All units are per-window billable totals: vcpu_seconds, mem_gib_seconds, and
-- storage_gib_hours are the time-integrated rate levels; egress_bytes and
-- gpu_seconds are the cumulative-counter deltas. The CoW deduplication has
-- already been applied upstream (usage.SamplesFromReport), so summing these rows
-- reconstructs the CoW-aware bill, never the naive per-fork double count.

CREATE TABLE usage_records (
    org_id            TEXT             NOT NULL,
    sandbox_id        TEXT             NOT NULL,
    window_start      TIMESTAMPTZ      NOT NULL,
    vcpu_seconds      DOUBLE PRECISION NOT NULL DEFAULT 0,
    mem_gib_seconds   DOUBLE PRECISION NOT NULL DEFAULT 0,
    storage_gib_hours DOUBLE PRECISION NOT NULL DEFAULT 0,
    egress_bytes      BIGINT           NOT NULL DEFAULT 0,
    gpu_seconds       BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, sandbox_id, window_start)
);

-- The usage API lists an org's records over a half-open [from, to) window,
-- ordered by (sandbox_id, window_start); this index serves that org-scoped,
-- period-bounded read.
CREATE INDEX usage_records_org_window_idx ON usage_records (org_id, window_start);
