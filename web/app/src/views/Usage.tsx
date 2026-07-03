// Usage view: renders totals and cost maps as StatTiles. Shows an honest empty
// state when all totals are zero. Consumes the live BFF via useUsage().
import { useUsage } from '../data/account'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { StatTile } from '../ui/StatTile'
import { PageHeader } from '../ui/PageHeader'

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
      <PageHeader title="Usage" lede="Consumption for the current billing period. These numbers are measured live from your sandboxes; nothing here is estimated." />

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
