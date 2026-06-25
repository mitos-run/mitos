// The cockpit: the org's OWN measured numbers, not a welcome screen. Activate
// latency (warm-claim P50/P99, their cluster), CoW density (memory saved by
// page-sharing) and marginal bytes per fork, forks served. Every headline metric
// carries a "Reproduce this" affordance pointing at the in-repo bench. No number
// is invented here; all values come from /console/instruments.
import { useInstruments } from '../data/instruments'
import { StatTile } from '../ui/StatTile'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { fmtBytes } from '../api'

const BENCH = 'bench/husk-activate-latency.sh'

export function Instruments() {
  const { data, isLoading, error } = useInstruments()
  if (error) return <EmptyState title="Overview unavailable" body="The telemetry pipeline could not be read for this organization." />
  if (isLoading || !data) return <Skeleton rows={4} />

  const noData = data.forks_served === 0 && data.activate_p50_ms === 0
  if (noData) {
    return (
      <EmptyState
        title="No measured signal yet"
        body="Fork a sandbox to see this org's activate latency and CoW density here. Only measured signal emits light."
      />
    )
  }

  return (
    <section>
      <h2>Overview</h2>
      <p className="t-dim">This organization's measured signal. Every number is reproducible.</p>
      <div className="cockpit-grid">
        <StatTile label="Activate P50" value={String(Math.round(data.activate_p50_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Activate P99" value={String(Math.round(data.activate_p99_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="CoW savings" value={fmtBytes(data.cow_savings_bytes)} hint="memory not spent, forks share parent pages" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Marginal / fork" value={fmtBytes(data.marginal_bytes_per_fork)} hint="mean private-dirty set per fork" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Forks served" value={String(data.forks_served)} hint="total for this org" />
      </div>
    </section>
  )
}
