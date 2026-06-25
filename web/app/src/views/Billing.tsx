// Billing view: status badge, balance and spend (cents to dollars), cap info,
// a ledger table (Time, Amount, Reason), and a "Manage billing" button that
// calls api.billingPortal() and opens the returned URL. Gated on c.billing.
import { useBilling } from '../data/account'
import { api } from '../api'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { StatTile } from '../ui/StatTile'

function fmtDollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

export function Billing() {
  const { data, isLoading } = useBilling()

  async function onManageBilling() {
    const url = await api.billingPortal()
    window.open(url, '_blank')
  }

  return (
    <section>
      <h2>Billing</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Balance, spend, and ledger for this org.
      </p>

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

          <h3 style={{ marginBottom: 'var(--space-3)' }}>Ledger</h3>
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
