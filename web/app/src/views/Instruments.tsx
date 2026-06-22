// The instrument-panel home (#276): the org's OWN measured Pareto metrics. No
// fabricated competitor numbers — only what the server measured.
import { useEffect, useState } from 'react'
import { Card, Division } from '@mitos/brand'
import { api, fmtBytes, type Instruments as I } from '../api'

function Metric({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <Card style={{ flex: 1, minWidth: 180 }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.04em' }}>{label}</div>
      <div style={{ fontSize: 'var(--step-3)', fontFamily: 'var(--mono)' }}>{value}</div>
      {sub && <div className="t-dim" style={{ fontSize: 'var(--step--1)' }}>{sub}</div>}
    </Card>
  )
}

export function Instruments() {
  const [d, setD] = useState<I | null>(null)
  const [err, setErr] = useState<string>()
  useEffect(() => {
    api.instruments().then(setD).catch((e) => setErr(String(e)))
  }, [])
  if (err) return <div className="t-dim">instruments unavailable: {err}</div>
  if (!d) return <div className="t-dim">measuring…</div>
  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)', marginBottom: 'var(--space-5)' }}>
        <Division size={40} />
        <div>
          <h2 style={{ margin: 0 }}>Instruments</h2>
          <div className="t-dim" style={{ fontSize: 'var(--step--1)' }}>measured on your cluster · reproduce with bench/husk-activate-latency.sh</div>
        </div>
      </div>
      <div style={{ display: 'flex', gap: 'var(--space-4)', flexWrap: 'wrap' }}>
        <Metric label="Warm-claim activate" value={`${d.activate_p50_ms} ms`} sub={`p99 ${d.activate_p99_ms} ms`} />
        <Metric label="Forks served" value={`${d.forks_served}`} />
        <Metric label="CoW savings" value={fmtBytes(d.cow_savings_bytes)} sub="memory not spent via page sharing" />
        <Metric label="Marginal / fork" value={fmtBytes(d.marginal_bytes_per_fork)} sub="private-dirty per fork" />
      </div>
    </div>
  )
}
