// Usage view: renders totals and cost maps as StatTiles. Shows an honest empty
// state when all totals are zero. Consumes the live BFF via useUsage().
import { useUsage } from '../data/account'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { StatTile } from '../ui/StatTile'

function fmtLabel(key: string): string {
  return key.replace(/_/g, ' ')
}

function fmtCents(n: number): string {
  return `$${(n / 100).toFixed(2)}`
}

export function Usage() {
  const { data, isLoading } = useUsage()

  const totals = data?.totals ?? {}
  const cost = data?.cost ?? {}
  const hasData = Object.values(totals).some((v) => v > 0)

  return (
    <section>
      <h2>Usage</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Consumption metrics for the current billing period. All numbers come directly from the BFF.
      </p>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : !hasData ? (
        <EmptyState
          title="No usage yet"
          body="Metrics will appear here once sandboxes have been run."
        />
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 'var(--space-4)' }}>
          {Object.entries(totals).map(([key, value]) => (
            <StatTile
              key={key}
              label={fmtLabel(key)}
              value={String(value)}
            />
          ))}
          {Object.entries(cost).map(([key, value]) => (
            <StatTile
              key={`cost-${key}`}
              label={fmtLabel(key)}
              value={fmtCents(value)}
            />
          ))}
        </div>
      )}
    </section>
  )
}
