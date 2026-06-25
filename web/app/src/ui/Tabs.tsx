// Accessible tab bar: ARIA tablist with roving focus and arrow-key navigation.
// The caller owns the active key and renders the panel; this is presentation +
// keyboard behavior only.
export type TabDef = { key: string; label: string }

export function Tabs({ tabs, active, onChange }: { tabs: TabDef[]; active: string; onChange: (key: string) => void }) {
  function onKey(e: React.KeyboardEvent, i: number) {
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      e.preventDefault()
      const next = e.key === 'ArrowRight' ? (i + 1) % tabs.length : (i - 1 + tabs.length) % tabs.length
      onChange(tabs[next].key)
    }
  }
  return (
    <div role="tablist" className="tabs" aria-label="Sandbox detail sections">
      {tabs.map((t, i) => (
        <button
          key={t.key}
          role="tab"
          id={`tab-${t.key}`}
          aria-selected={t.key === active}
          aria-controls={`panel-${t.key}`}
          tabIndex={t.key === active ? 0 : -1}
          className={`tab ${t.key === active ? 'tab-active' : ''}`}
          onClick={() => onChange(t.key)}
          onKeyDown={(e) => onKey(e, i)}
        >
          {t.label}
        </button>
      ))}
    </div>
  )
}
