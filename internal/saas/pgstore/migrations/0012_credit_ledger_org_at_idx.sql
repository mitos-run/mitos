-- Index for the month-scoped ledger read (issue #615): the drawdown driver's
-- per-cycle spend-cap evaluation reads each capped org's CURRENT-MONTH entries
-- (billing.ScopedLedgerReader / PgCreditLedger.EntriesSince) instead of the
-- org's lifetime history. Without this index that read is a per-org seq scan
-- whose cost grows with ledger lifetime; with it the read is bounded by the
-- month's rows.
CREATE INDEX IF NOT EXISTS credit_ledger_org_at_idx ON credit_ledger (org_id, at);
