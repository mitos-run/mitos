// Live sandboxes + the fork-tree centerpiece. The fork tree is the on-brand,
// uniquely-mitos view: a magenta dividing membrane (fork) around the shared
// cyan parent snapshot. Here it renders the org's live sandboxes grouped by
// template as a simple division diagram; the live CoW annotations land when the
// real SandboxControl cluster query is wired (blocked on the tenancy track).
import { useEffect, useState } from 'react'
import { Card, Division, StatusDot } from '@mitos/brand'
import { api, type SandboxView } from '../api'

export function Sandboxes() {
  const [rows, setRows] = useState<SandboxView[]>([])
  const [err, setErr] = useState<string>()
  useEffect(() => {
    api.sandboxes().then(setRows).catch((e) => setErr(String(e)))
  }, [])

  const byTemplate = rows.reduce<Record<string, SandboxView[]>>((acc, s) => {
    ;(acc[s.template] ??= []).push(s)
    return acc
  }, {})

  return (
    <div>
      <h2>Sandboxes</h2>
      {err && <div className="t-dim">{err}</div>}

      <Card style={{ marginBottom: 'var(--space-5)' }}>
        <div className="t-dim" style={{ fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.04em', marginBottom: 'var(--space-3)' }}>Fork tree</div>
        {Object.entries(byTemplate).map(([tmpl, kids]) => (
          <div key={tmpl} style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)', marginBottom: 'var(--space-3)' }}>
            <Division size={36} />
            <div>
              <div className="mono"><StatusDot entity="parent" /> {tmpl} <span className="t-dim">(shared snapshot)</span></div>
              <div style={{ display: 'flex', gap: 'var(--space-2)', marginTop: 'var(--space-1)', flexWrap: 'wrap' }}>
                {kids.map((k) => (
                  <span key={k.id} className="mono t-fork" style={{ fontSize: 'var(--step--1)' }}><StatusDot entity="fork" /> {k.id}</span>
                ))}
              </div>
            </div>
          </div>
        ))}
        {rows.length === 0 && <div className="t-dim">no live sandboxes</div>}
      </Card>

      <table className="tbl">
        <thead>
          <tr><th>ID</th><th>Template</th><th>Node</th><th>Phase</th><th>vCPUs</th></tr>
        </thead>
        <tbody>
          {rows.map((s) => (
            <tr key={s.id}>
              <td className="mono">{s.id}</td>
              <td>{s.template}</td>
              <td>{s.node}</td>
              <td><StatusDot entity={s.phase === 'Running' ? 'ready' : 'warn'} /> {s.phase}</td>
              <td>{s.vcpus}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
