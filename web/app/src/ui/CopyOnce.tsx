// One-time secret reveal: shows a value with a copy button and an explicit
// "shown once" warning. Used for the raw API key on create. The caller is
// responsible for never persisting the value.
import { useState } from 'react'

export function CopyOnce({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="card" style={{ borderColor: 'var(--amber)' }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)' }}>{label} (shown once, store it now)</div>
      <div style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'center', marginTop: 'var(--space-2)' }}>
        <code className="mono" style={{ flex: 1, overflowX: 'auto' }}>{value}</code>
        <button className="btn btn-ghost" onClick={() => { void navigator.clipboard.writeText(value); setCopied(true) }}>{copied ? 'Copied' : 'Copy'}</button>
      </div>
    </div>
  )
}
