-- Per-org drawdown remainder (issue #662): the carried sub-cent balance of the
-- milli-cent usage accumulator. Pricing usage per record in whole cents rounded
-- every realistic window to zero, so drawdown settled nothing forever; the
-- accumulator prices in milli-cents, settles whole cents into the cents-based
-- credit_ledger, and carries the remainder here. One row per org, upserted
-- ATOMICALLY in the same transaction as the ledger debit (PgCreditLedger.
-- AppendWithRemainder), so a replayed window or a crash can never skew the
-- carry against the ledger.
--
-- milli_cents is SIGNED: the settle rounds half up, so the value stays in
-- (-500, 500); negative means the org prepaid a sub-cent that offsets future
-- usage. The CHECK bounds it defensively at under one cent either way.
CREATE TABLE drawdown_remainders (
    org_id      TEXT        PRIMARY KEY,
    milli_cents INTEGER     NOT NULL CHECK (milli_cents > -1000 AND milli_cents < 1000),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
