-- Processed usage windows (issue #672): the drawdown's idempotency markers,
-- moved OUT of the customer-visible credit ledger. Before this table every
-- priced usage record wrote a keyed credit_ledger row even when the settled
-- amount was 0 cents, so ~94 percent of usage_drawdown rows were zero-amount
-- markers and every 5m tick re-priced the whole 2h lookback before the ledger
-- key rejected the replay. One row here marks one settled (org, sandbox,
-- window); the drawdown driver skips marked windows BEFORE pricing, and the
-- credit ledger only carries rows that actually moved money.
--
-- The row is written ATOMICALLY with the ledger debit and the drawdown
-- remainder (PgCreditLedger.SettleWindow, the extended #666 single-transaction
-- path), so a crash can never land a debit without its marker or vice versa.
--
-- Rows are pruned once window_at falls out of the drawdown lookback horizon:
-- the driver never lists a window that old again, so the marker has nothing
-- left to guard and the table stays bounded (lookback / usage-window rows per
-- active sandbox).
CREATE TABLE processed_usage_windows (
    org_id     TEXT        NOT NULL,
    sandbox_id TEXT        NOT NULL,
    window_at  TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, sandbox_id, window_at)
);

-- The pruning delete filters on window_at alone; the primary key only helps
-- org-prefixed lookups.
CREATE INDEX processed_usage_windows_window_idx ON processed_usage_windows (window_at);
