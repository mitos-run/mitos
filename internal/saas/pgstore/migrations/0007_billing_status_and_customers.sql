-- Durable billing state (issue #614) so a console restart cannot revert an org
-- suspended for nonpayment to active, or orphan a paid credit top-up.
--
-- billing_status holds each org's dunning state (billing.BillingStatus: active,
-- past_due, suspended). One row per org; SetStatus upserts because provider
-- webhooks replay. An org with no row reads as "active" at the store boundary,
-- exactly like the in-memory store.
CREATE TABLE billing_status (
    org_id     TEXT        PRIMARY KEY,
    status     TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- billing_customers is the bidirectional org <-> provider-customer map: the
-- webhook resolves customer to org, the portal and top-up links resolve org to
-- customer. Exactly one payment provider is active per deployment (selected at
-- startup), so the customer ref carries no provider column. Link replaces any
-- stale row on either side inside one transaction, so a replayed or re-issued
-- link is idempotent and both unique constraints hold without insert errors.
CREATE TABLE billing_customers (
    org_id       TEXT        PRIMARY KEY,
    customer_ref TEXT        NOT NULL UNIQUE,
    linked_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
