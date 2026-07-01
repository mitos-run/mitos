// Billing view: status badge, balance and spend (cents to dollars), cap info,
// a spend-cap form (soft/hard in dollars), a ledger table (Time, Amount,
// Reason), and a "Manage billing" button. Gated on c.billing.
import { useState } from 'react'
import { useBilling, useSetSpendCap } from '../data/account'
import { api } from '../api'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { StatTile } from '../ui/StatTile'
import { PageHeader } from '../ui/PageHeader'

function fmtDollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

// dollarsToCents converts a dollar-string entered by the user to integer cents.
// Returns 0 for empty or non-numeric input.
function dollarsToCents(val: string): number {
  const n = parseFloat(val)
  if (!isFinite(n) || n < 0) return 0
  return Math.round(n * 100)
}

export function Billing() {
  const { data, isLoading } = useBilling()
  const setSpendCap = useSetSpendCap()

  // Form state: dollar amounts the user types; API receives integer cents.
  const [softDollars, setSoftDollars] = useState('')
  const [hardDollars, setHardDollars] = useState('')
  const [capSaved, setCapSaved] = useState(false)

  function onSpendCapSubmit(e: React.FormEvent) {
    e.preventDefault()
    const softCents = dollarsToCents(softDollars)
    const hardCents = dollarsToCents(hardDollars)
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

  return (
    <section>
      <PageHeader title="Billing" lede="Balance, spend, and ledger for this org." />

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

          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))', gap: 'var(--space-4)', marginBottom: 'var(--space-6)' }}>
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
          <form onSubmit={onSpendCapSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 360, marginBottom: 'var(--space-6)' }}>
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
                onChange={(e) => { setSoftDollars(e.target.value); setCapSaved(false) }}
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
                onChange={(e) => { setHardDollars(e.target.value); setCapSaved(false) }}
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
                  style={{ fontSize: 'var(--step--1)', color: 'var(--color-ok, green)' }}
                >
                  Spend cap saved.
                </span>
              )}
            </div>
          </form>

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
                      <td className="t-dim">{entry.ts ? new Date(entry.ts).toLocaleString() : '-'}</td>
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
