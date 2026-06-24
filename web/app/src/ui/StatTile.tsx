// The instrument tile: one measured number with its label and unit, and an
// optional "Reproduce this" disclosure that names the in-repo bench command.
// Integrity as a feature: every headline metric can be reproduced. Value strings
// are formatted by the caller from real BFF data; this primitive never invents a
// number.
import { useState } from 'react'
import { Card } from '@mitos/brand'

export type StatTileProps = {
  label: string
  value: string
  unit?: string
  hint?: string
  reproduce?: { label: string; command: string }
}

export function StatTile({ label, value, unit, hint, reproduce }: StatTileProps) {
  const [open, setOpen] = useState(false)
  return (
    <Card style={{ padding: 'var(--space-5)' }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.08em' }}>{label}</div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)', marginTop: 'var(--space-2)' }}>
        <span style={{ fontSize: 'var(--step-4)', fontFamily: 'var(--mono)', color: 'var(--cyan)' }}>{value}</span>
        {unit && <span className="t-dim">{unit}</span>}
      </div>
      {hint && <div className="t-dim" style={{ fontSize: 'var(--step--1)', marginTop: 'var(--space-2)' }}>{hint}</div>}
      {reproduce && (
        <div style={{ marginTop: 'var(--space-3)' }}>
          <button
            className="btn btn-ghost"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
            style={{ fontSize: 'var(--step--1)' }}
          >
            {reproduce.label}
          </button>
          {open && (
            <pre className="t-dim" style={{ marginTop: 'var(--space-2)', fontSize: 'var(--step--1)', overflowX: 'auto' }}>
              <code>{reproduce.command}</code>
            </pre>
          )}
        </div>
      )}
    </Card>
  )
}
