-- Durable per-org spend caps so a cap set in the console survives a controller
-- restart or redeploy (money-safety: the in-memory store dropped caps on every
-- restart). One row per org; Set upserts. Amounts are integer cents (BIGINT),
-- matching billing.Money. A zero cap means "no cap" at the read boundary, exactly
-- as the in-memory store and the console reader already treat it.
CREATE TABLE spend_caps (
    org_id   TEXT   PRIMARY KEY,
    soft_cap BIGINT NOT NULL DEFAULT 0,
    hard_cap BIGINT NOT NULL DEFAULT 0
);
