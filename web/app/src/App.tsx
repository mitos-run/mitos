// The console shell: boots on /console/capabilities, then mounts a
// capability-gated nav. The two editions download THIS SAME bundle; only the
// capabilities document differs, so the Billing route (and signup, org switcher)
// appear only when the server advertises them.
import { useEffect, useState } from 'react'
import { Division } from '@mitos/brand'
import { api, type Capabilities } from './api'
import { Instruments } from './views/Instruments'
import { Secrets } from './views/Secrets'
import { Sandboxes } from './views/Sandboxes'

type View = { key: string; label: string; render: () => JSX.Element; when?: (c: Capabilities) => boolean }

// Compact stub for views whose rich UI is a follow-up; each still names its BFF
// source so the shell is honest about what is wired.
function Stub({ name, endpoint }: { name: string; endpoint: string }) {
  return (
    <div>
      <h2>{name}</h2>
      <p className="t-dim">Reads <code>{endpoint}</code>. Rich view is a follow-up; the org-scoped BFF endpoint is live.</p>
    </div>
  )
}

const VIEWS: View[] = [
  { key: 'instruments', label: 'Instruments', render: () => <Instruments />, when: (c) => c.proof },
  { key: 'sandboxes', label: 'Sandboxes', render: () => <Sandboxes /> },
  { key: 'secrets', label: 'Secrets', render: () => <Secrets /> },
  { key: 'keys', label: 'API keys', render: () => <Stub name="API keys" endpoint="/console/keys" /> },
  { key: 'usage', label: 'Usage', render: () => <Stub name="Usage & cost" endpoint="/console/usage" /> },
  { key: 'templates', label: 'Templates', render: () => <Stub name="Templates" endpoint="/console/templates" /> },
  { key: 'members', label: 'Members', render: () => <Stub name="Members" endpoint="/console/members" />, when: (c) => c.teams },
  { key: 'audit', label: 'Audit', render: () => <Stub name="Audit log" endpoint="/console/audit" /> },
  { key: 'billing', label: 'Billing', render: () => <Stub name="Billing" endpoint="/console/billing" />, when: (c) => c.billing },
]

export function App() {
  const [caps, setCaps] = useState<Capabilities | null>(null)
  const [active, setActive] = useState('instruments')
  const [err, setErr] = useState<string>()

  useEffect(() => {
    api.capabilities().then(setCaps).catch((e) => setErr(String(e)))
  }, [])

  if (err) return <main style={{ padding: 32 }}><div className="t-dim">console unavailable: {err}</div></main>
  if (!caps) return <main style={{ padding: 32 }}><div className="t-dim">loading…</div></main>

  const nav = VIEWS.filter((v) => !v.when || v.when(caps))
  const current = nav.find((v) => v.key === active) ?? nav[0]

  return (
    <>
      <div className="field" />
      <div style={{ position: 'relative', display: 'flex', minHeight: '100vh', maxWidth: 'var(--maxw)', margin: '0 auto' }}>
        <nav style={{ width: 200, padding: 'var(--space-5)', borderRight: '1px solid var(--hairline)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', marginBottom: 'var(--space-6)' }}>
            <Division size={28} />
            <strong>mitos</strong>
          </div>
          {nav.map((v) => (
            <button
              key={v.key}
              className={`btn ${v.key === current.key ? 'btn-primary' : 'btn-ghost'}`}
              style={{ display: 'block', width: '100%', textAlign: 'left', marginBottom: 'var(--space-2)' }}
              onClick={() => setActive(v.key)}
            >
              {v.label}
            </button>
          ))}
          <OwnershipBadge caps={caps} />
        </nav>
        <main style={{ flex: 1, padding: 'var(--space-6)' }}>{current.render()}</main>
      </div>
    </>
  )
}

// The chrome badge competitors structurally can't match: self-host emphasizes
// data residency; hosted emphasizes portability + no lock-in.
function OwnershipBadge({ caps }: { caps: Capabilities }) {
  const selfHosted = caps.ownership === 'self-hosted'
  return (
    <div className="card" style={{ marginTop: 'var(--space-6)', fontSize: 'var(--step--1)' }}>
      <div style={{ color: 'var(--cyan)' }}>{selfHosted ? 'Self-hosted' : 'Hosted by mitos'}</div>
      <div className="t-dim">{selfHosted ? 'Your data never leaves your infrastructure.' : 'Same engine & API · portable to self-host.'}</div>
    </div>
  )
}
