// Billing view: plan card, status badge, balance and spend (cents to
// dollars), cap info, a spend-cap form (soft/hard in dollars), an add-credits
// section (preset tiers plus a custom amount), a Boxes section (the reserved-
// capacity catalog), a ledger table (Time, Amount, Reason), and a "Manage
// billing" button. Gated on c.billing.
import { useState } from 'react'
import { useBilling, useSetSpendCap, useBoxes } from '../data/account'
import { useAccount } from '../data/account-settings'
import { useCapabilities } from '../data/query'
import { api } from '../api'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { StatTile } from '../ui/StatTile'
import { fmtAbsolute } from '../lib/dates'
import { PageHeader } from '../ui/PageHeader'

function fmtDollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

// dollarsToCents converts a dollar-string entered by the user to integer cents.
// Returns 0 for an empty string (meaning "not set" for that threshold).
// Returns null when the input is non-empty but not a valid non-negative number;
// callers must treat null as an invalid entry and block submission.
function dollarsToCents(val: string): number | null {
  if (val.trim() === '') return 0
  const n = parseFloat(val)
  if (!isFinite(n) || n < 0) return null
  return Math.round(n * 100)
}

// Preset top-up tiers. Labels are derived from fmtDollars so formatting stays
// consistent with the rest of the billing view.
const TOPUP_TIERS: Array<{ cents: number; label: string }> = [
  { cents: 1000, label: fmtDollars(1000) },
  { cents: 2500, label: fmtDollars(2500) },
  { cents: 5000, label: fmtDollars(5000) },
  { cents: 10000, label: fmtDollars(10000) },
]

const PLAN_LABELS: Record<string, string> = { free: 'Free', team: 'Team' }

// PlanCard shows the caller's org's current plan and the entitlements it
// unlocks, from capabilities (server-resolved per org). It renders nothing
// until capabilities with a plan have loaded, so it never flashes a wrong
// plan.
function PlanCard() {
  const { data: caps } = useCapabilities()
  if (!caps?.plan || !caps.entitlements) return null
  const e = caps.entitlements
  const label = PLAN_LABELS[caps.plan] ?? caps.plan

  return (
    <section style={{ marginBottom: 'var(--space-6)' }}>
      <h2 style={{ marginBottom: 'var(--space-3)' }}>Plan</h2>
      <div className="card" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', maxWidth: 420 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <span
            className="mono"
            style={{ padding: 'var(--space-1) var(--space-3)', borderRadius: 'var(--r-sm)', background: 'var(--field-1)', fontSize: 'var(--step--1)' }}
          >
            {label}
          </span>
          {caps.edition === 'community' && (
            <span className="t-dim" style={{ fontSize: 'var(--step--1)' }}>
              Self-hosted: every feature is included, no plan required.
            </span>
          )}
        </div>
        <ul className="t-dim" style={{ margin: 0, paddingLeft: 'var(--space-5)', fontSize: 'var(--step--1)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          <li>SSO enforcement: {e.ssoEnforced ? 'on' : 'off'}</li>
          <li>SCIM provisioning: {e.scim ? 'on' : 'off'}</li>
          <li>Audit-sink streaming: {e.auditStreaming ? 'on' : 'off'}</li>
          <li>Audit retention: {e.auditRetentionDays > 0 ? `${e.auditRetentionDays} days` : 'unlimited'}</li>
        </ul>
        {caps.edition === 'hosted' && caps.plan === 'free' && (
          <p className="t-dim" style={{ fontSize: 'var(--step--1)', margin: 0 }}>
            The Team plan adds SSO enforcement, SCIM, and audit-sink streaming with extended retention.
          </p>
        )}
      </div>
    </section>
  )
}

// BoxesSection lists the Box reservation catalog (illustrative pricing) and
// an honest purchase-path state: when a top-up provider is configured it
// deep-links the billing portal (the same seam the "Manage billing" button
// uses); otherwise it shows a calm contact message rather than a fake
// purchase flow.
function BoxesSection({ topUpAvailable, onManageBilling }: { topUpAvailable: boolean; onManageBilling: () => void }) {
  const { data: boxes, isLoading } = useBoxes()

  return (
    <>
      <h2 style={{ marginBottom: 'var(--space-3)' }}>Boxes</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
        Reserved monthly capacity, billed at a discount to pay-as-you-go usage. Pricing is illustrative.
      </p>
      {isLoading ? (
        <Skeleton rows={3} />
      ) : !boxes || boxes.length === 0 ? (
        <EmptyState title="No boxes available" body="The reserved-capacity catalog could not be loaded." />
      ) : (
        <div className="stat-grid" style={{ marginBottom: 'var(--space-4)' }}>
          {boxes.map((b) => (
            <div key={b.key} className="card" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              <strong>{b.vcpu} vCPU / {b.mem_gib} GiB</strong>
              <span className="mono">{fmtDollars(b.monthly_cents)}/mo</span>
            </div>
          ))}
        </div>
      )}
      {topUpAvailable ? (
        <button className="btn" onClick={onManageBilling} style={{ marginBottom: 'var(--space-6)' }}>
          Manage in billing portal
        </button>
      ) : (
        <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-6)' }}>
          Contact us to reserve a box while self-serve purchase is built.
        </p>
      )}
    </>
  )
}

export function Billing() {
  const { data, isLoading } = useBilling()
  const { data: account } = useAccount()
  const setSpendCap = useSetSpendCap()

  // Spend-cap form state: dollar amounts the user types; API receives integer cents.
  const [softDollars, setSoftDollars] = useState('')
  const [hardDollars, setHardDollars] = useState('')
  const [capSaved, setCapSaved] = useState(false)
  // capInputError is set when a field contains text that is not a valid
  // non-negative dollar amount. It is cleared when the user changes either field.
  const [capInputError, setCapInputError] = useState<string | null>(null)

  // Add-credits state: the custom dollar amount field, any top-up error, and
  // a pending flag that disables all top-up controls while a checkout call is
  // in-flight to prevent double-submit.
  const [customTopUp, setCustomTopUp] = useState('')
  const [topUpError, setTopUpError] = useState<string | null>(null)
  const [topUpPending, setTopUpPending] = useState(false)

  function onSpendCapSubmit(e: React.FormEvent) {
    e.preventDefault()
    const softCents = dollarsToCents(softDollars)
    const hardCents = dollarsToCents(hardDollars)
    if (softCents === null || hardCents === null) {
      setCapInputError('Enter a valid dollar amount, or leave the field empty to remove that cap.')
      return
    }
    setCapInputError(null)
    setCapSaved(false)
    setSpendCap.mutate(
      { softCents, hardCents },
      {
        onSuccess: () => setCapSaved(true),
      },
    )
  }

  async function onManageBilling() {
    const url = await api.billingPortal()
    window.open(url, '_blank')
  }

  // startTopUp opens the hosted checkout for the given integer cent amount.
  // Guards against double-submit by returning early when a call is already in
  // flight. On failure it shows a calm inline message; never throws.
  async function startTopUp(cents: number) {
    if (topUpPending) return
    setTopUpPending(true)
    setTopUpError(null)
    try {
      const url = await api.topupUrl(cents)
      window.open(url, '_blank')
    } catch {
      setTopUpError('Credits checkout could not be started. Please try again.')
    } finally {
      setTopUpPending(false)
    }
  }

  function onCustomTopUpSubmit(e: React.FormEvent) {
    e.preventDefault()
    const cents = dollarsToCents(customTopUp)
    // Reject null (invalid input) and 0 (empty field or literal zero).
    if (cents === null || cents === 0) {
      setTopUpError('Enter a valid dollar amount greater than zero.')
      return
    }
    void startTopUp(cents)
  }

  return (
    <section>
      <PageHeader title="Billing" lede="Balance, spend, and ledger for this org." />

      <PlanCard />

      {isLoading ? (
        <Skeleton rows={4} />
      ) : !data ? (
        <EmptyState
          title="Billing unavailable"
          body="Billing information could not be loaded."
        />
      ) : (
        <>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)', marginBottom: 'var(--space-5)', flexWrap: 'wrap' }}>
            <span
              className="mono"
              style={{ padding: 'var(--space-1) var(--space-3)', borderRadius: 'var(--r-sm)', background: 'var(--field-1)', fontSize: 'var(--step--1)' }}
            >
              {data.status}
            </span>
            <button className="btn" onClick={() => void onManageBilling()}>
              Manage billing
            </button>
          </div>

          <div className="stat-grid" style={{ marginBottom: 'var(--space-6)' }}>
            <StatTile label="Balance" value={fmtDollars(data.balance_cents)} />
            <StatTile label="Spend" value={fmtDollars(data.spend_cents)} />
            <StatTile
              label="Soft cap"
              value={data.soft_cap_cents > 0 ? fmtDollars(data.soft_cap_cents) : 'No cap'}
            />
            <StatTile
              label="Hard cap"
              value={data.hard_cap_cents > 0 ? fmtDollars(data.hard_cap_cents) : 'No cap'}
            />
          </div>

          <h2 style={{ marginBottom: 'var(--space-4)' }}>Set spend cap</h2>
          <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
            A soft cap fires a budget alert. A hard cap suspends the org to prevent unbounded spend.
            Enter amounts in dollars. Leave a field at 0 to leave that threshold unset.
          </p>
          <form onSubmit={onSpendCapSubmit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 360, marginBottom: 'var(--space-6)' }}>
            <div>
              <label htmlFor="soft-cap-dollars" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                Soft cap (dollars)
              </label>
              <input
                id="soft-cap-dollars"
                type="number"
                min={0}
                step="0.01"
                placeholder="0"
                value={softDollars}
                onChange={(e) => { setSoftDollars(e.target.value); setCapSaved(false); setCapInputError(null) }}
                style={{ width: '140px' }}
              />
            </div>
            <div>
              <label htmlFor="hard-cap-dollars" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                Hard cap (dollars)
              </label>
              <input
                id="hard-cap-dollars"
                type="number"
                min={0}
                step="0.01"
                placeholder="0"
                value={hardDollars}
                onChange={(e) => { setHardDollars(e.target.value); setCapSaved(false); setCapInputError(null) }}
                style={{ width: '140px' }}
              />
            </div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
              <button type="submit" className="btn btn-primary" disabled={setSpendCap.isPending}>
                {setSpendCap.isPending ? 'Saving...' : 'Save spend cap'}
              </button>
              {capSaved && (
                <span
                  role="status"
                  aria-live="polite"
                  style={{ fontSize: 'var(--step--1)', color: 'var(--green)' }}
                >
                  Spend cap saved.
                </span>
              )}
              {capInputError && (
                <span
                  role="alert"
                  aria-live="assertive"
                  style={{ fontSize: 'var(--step--1)', color: 'var(--amber)' }}
                >
                  {capInputError}
                </span>
              )}
              {setSpendCap.isError && !capInputError && (
                <span
                  role="alert"
                  aria-live="assertive"
                  style={{ fontSize: 'var(--step--1)', color: 'var(--amber)' }}
                >
                  The spend cap could not be saved. Please try again.
                </span>
              )}
            </div>
          </form>

          <h2 style={{ marginBottom: 'var(--space-4)' }}>Add credits</h2>
          {data.topup_available ? (
            <>
              <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
                Buy prepaid credits for this org. Choose a tier or enter a custom amount.
              </p>
              {/* Single shared aria-live region for all top-up errors (preset and custom). */}
              {topUpError && (
                <span
                  role="alert"
                  aria-live="assertive"
                  style={{ display: 'block', fontSize: 'var(--step--1)', color: 'var(--amber)', marginBottom: 'var(--space-3)' }}
                >
                  {topUpError}
                </span>
              )}
              <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap', marginBottom: 'var(--space-4)' }}>
                {TOPUP_TIERS.map(({ cents, label }) => (
                  <button key={cents} className="btn" disabled={topUpPending} onClick={() => void startTopUp(cents)}>
                    {label}
                  </button>
                ))}
              </div>
              <form onSubmit={onCustomTopUpSubmit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 360, marginBottom: 'var(--space-6)' }}>
                <div>
                  <label htmlFor="custom-topup-dollars" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                    Custom amount (dollars)
                  </label>
                  <input
                    id="custom-topup-dollars"
                    type="number"
                    min={0.01}
                    step="0.01"
                    placeholder="0"
                    value={customTopUp}
                    onChange={(e) => { setCustomTopUp(e.target.value); setTopUpError(null) }}
                    style={{ width: '140px' }}
                  />
                </div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
                  <button type="submit" className="btn btn-primary" disabled={topUpPending}>
                    Add credits
                  </button>
                </div>
              </form>
            </>
          ) : (
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-6)' }}>
              Adding credits is not available yet. Please check back soon.
            </p>
          )}

          <BoxesSection topUpAvailable={data.topup_available} onManageBilling={() => void onManageBilling()} />

          <h2 style={{ marginBottom: 'var(--space-3)' }}>Ledger</h2>
          {data.ledger_entries.length === 0 ? (
            <EmptyState
              title="No ledger entries"
              body="Charges and credits will appear here."
            />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table className="tbl" aria-label="Ledger">
                <thead>
                  <tr>
                    <th scope="col">Time</th>
                    <th scope="col">Amount</th>
                    <th scope="col">Reason</th>
                  </tr>
                </thead>
                <tbody>
                  {data.ledger_entries.map((entry, i) => (
                    <tr key={i}>
                      <td className="t-dim">{entry.ts ? fmtAbsolute(entry.ts, account?.locale, account?.timezone) : '-'}</td>
                      <td className="mono">{entry.cents != null ? fmtDollars(entry.cents) : '-'}</td>
                      <td>{entry.reason ?? '-'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
