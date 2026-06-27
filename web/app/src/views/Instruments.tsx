// The operational home: proof-hero band of the org's own measured numbers
// (activate latency, CoW density, forks served) plus three operational panels
// (running sandboxes, spend this month, recent activity). Every number is real:
// the proof tiles come from /console/instruments, the panels from their own
// endpoints. No number is invented here.
import { Link } from '@tanstack/react-router'
import { useInstruments } from '../data/instruments'
import { useSandboxes } from '../data/sandboxes'
import { useBilling, useAudit } from '../data/account'
import { useCapabilities } from '../data/query'
import { StatTile } from '../ui/StatTile'
import { Skeleton } from '../ui/Skeleton'
import { fmtBytes, fmtDollars } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { Card } from '@mitos/brand'
import type { AuditEvent } from '../api'
import { FirstRun, isFirstRun } from './firstrun/FirstRun'

const BENCH = 'bench/husk-activate-latency.sh'

function fmtRelative(isoAt: string): string {
  // Short human-readable timestamp; real value from the event record.
  try {
    const d = new Date(isoAt)
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  } catch {
    return isoAt
  }
}

// ---- Proof hero band -------------------------------------------------------

function ProofHero() {
  const { data, isLoading, error } = useInstruments()

  if (error) {
    return (
      <p className="t-dim" style={{ marginTop: 'var(--space-4)', marginBottom: 'var(--space-5)' }}>
        Proof metrics unavailable.
      </p>
    )
  }
  if (isLoading || !data) return <Skeleton rows={2} />

  const noData = data.forks_served === 0 && data.activate_p50_ms === 0
  if (noData) {
    return (
      <p className="t-dim" style={{ marginTop: 'var(--space-4)', marginBottom: 'var(--space-5)' }}>
        No measured signal yet. Fork a sandbox to see activate latency and CoW density here.
      </p>
    )
  }

  return (
    <div className="cockpit-grid">
      <StatTile label="Activate P50" value={String(Math.round(data.activate_p50_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
      <StatTile label="Activate P99" value={String(Math.round(data.activate_p99_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
      <StatTile label="CoW savings" value={fmtBytes(data.cow_savings_bytes)} hint="memory not spent, forks share parent pages" reproduce={{ label: 'Reproduce this', command: BENCH }} />
      <StatTile label="Marginal / fork" value={fmtBytes(data.marginal_bytes_per_fork)} hint="mean private-dirty set per fork" reproduce={{ label: 'Reproduce this', command: BENCH }} />
      <StatTile label="Forks served" value={String(data.forks_served)} hint="total for this org" />
    </div>
  )
}

// ---- Running now panel -----------------------------------------------------

function RunningNowPanel() {
  const { data, isLoading } = useSandboxes()

  const running = (data ?? []).filter((s) => s.phase === 'Running')

  return (
    <Card>
      <h2 style={{ marginBottom: 'var(--space-4)' }}>Running now</h2>
      {isLoading ? (
        <Skeleton rows={2} />
      ) : running.length === 0 ? (
        <p className="t-dim">No sandboxes running.</p>
      ) : (
        <>
          <div style={{ fontSize: 'var(--step-4)', fontFamily: 'var(--mono)', color: 'var(--cyan)', marginBottom: 'var(--space-3)' }}>
            {running.length}
          </div>
          <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
            {running.slice(0, 5).map((s) => (
              <li key={s.id} className="mono" style={{ fontSize: 'var(--step--1)', padding: 'var(--space-1) 0', borderBottom: '1px solid var(--hairline)', color: 'var(--ink-2)' }}>
                {s.id}
              </li>
            ))}
          </ul>
        </>
      )}
      <div style={{ marginTop: 'var(--space-4)' }}>
        <Link to="/sandboxes" className="t-dim" style={{ fontSize: 'var(--step--1)', color: 'var(--cyan)', textDecoration: 'none' }}>
          View sandboxes
        </Link>
      </div>
    </Card>
  )
}

// ---- Spend this month panel ------------------------------------------------

function SpendPanel() {
  const { data, isLoading } = useBilling()

  return (
    <Card>
      <h2 style={{ marginBottom: 'var(--space-4)' }}>Spend this month</h2>
      {isLoading ? (
        <Skeleton rows={1} />
      ) : !data ? (
        <p className="t-dim">Billing data unavailable.</p>
      ) : (
        <div style={{ fontSize: 'var(--step-4)', fontFamily: 'var(--mono)', color: 'var(--cyan)' }}>
          {fmtDollars(data.spend_cents)}
        </div>
      )}
      <div style={{ marginTop: 'var(--space-4)' }}>
        <Link to="/billing" className="t-dim" style={{ fontSize: 'var(--step--1)', color: 'var(--cyan)', textDecoration: 'none' }}>
          View billing
        </Link>
      </div>
    </Card>
  )
}

// ---- Recent activity panel -------------------------------------------------

function RecentActivityPanel() {
  const { data, isLoading } = useAudit()

  const events: AuditEvent[] = data ?? []

  return (
    <Card>
      <h2 style={{ marginBottom: 'var(--space-4)' }}>Recent activity</h2>
      {isLoading ? (
        <Skeleton rows={3} />
      ) : events.length === 0 ? (
        <p className="t-dim">No activity yet.</p>
      ) : (
        <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          {events.slice(0, 5).map((e, i) => (
            <li key={i} style={{ padding: 'var(--space-2) 0', borderBottom: '1px solid var(--hairline)', fontSize: 'var(--step--1)' }}>
              <span style={{ color: 'var(--ink)' }}>{e.actor_id}</span>
              {' '}
              <span className="t-dim">{e.action}</span>
              {' '}
              <span className="mono" style={{ color: 'var(--ink-2)' }}>{e.target}</span>
              <span className="t-dim" style={{ float: 'right' }}>{fmtRelative(e.at)}</span>
            </li>
          ))}
        </ul>
      )}
      <div style={{ marginTop: 'var(--space-4)' }}>
        <Link to="/audit" className="t-dim" style={{ fontSize: 'var(--step--1)', color: 'var(--cyan)', textDecoration: 'none' }}>
          View audit log
        </Link>
      </div>
    </Card>
  )
}

// ---- Operational panels grid -----------------------------------------------

function OperationalPanels() {
  const { data: caps } = useCapabilities()

  return (
    <div className="overview-panels" style={{ marginTop: 'var(--space-7)' }}>
      <RunningNowPanel />
      {caps?.billing && <SpendPanel />}
      <RecentActivityPanel />
    </div>
  )
}

// ---- Overview (Instruments) ------------------------------------------------

export function Instruments() {
  const instruments = useInstruments()
  const sandboxes = useSandboxes()

  // Read ?uc from the URL for intent-shaped first-run content.
  // Falls back to undefined when the param is absent or SSR context.
  const uc =
    typeof window !== 'undefined'
      ? (new URLSearchParams(window.location.search).get('uc') ?? undefined)
      : undefined

  const showFirstRun = !instruments.isLoading && !sandboxes.isLoading && isFirstRun(instruments.data, sandboxes.data)

  return (
    <section>
      <PageHeader title="Overview" lede="This organization's measured signal, and what's happening right now." />
      {showFirstRun && <FirstRun uc={uc} />}
      <ProofHero />
      <OperationalPanels />
    </section>
  )
}
