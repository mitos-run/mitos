// A single button that cycles the explicit theme dark -> light -> system.
// 'system' removes data-theme so prefers-color-scheme decides (see appearance.ts);
// dark and light pin it explicitly. The icon and aria-label both reflect the
// theme that is about to become active, so a screen reader user hears what the
// button currently shows and what activating it will do next.
import { useState } from 'react'
import { getAppearance, setAppearance, type Theme } from '../appearance'

const NEXT: Record<Theme, Theme> = { dark: 'light', light: 'system', system: 'dark' }

function label(theme: Theme): string {
  return `Theme: ${theme}. Activate for ${NEXT[theme]}.`
}

function Glyph({ theme }: { theme: Theme }) {
  if (theme === 'dark') {
    return (
      <svg width="18" height="18" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" focusable="false">
        <path d="M17 12.5A7.5 7.5 0 0 1 7.5 3a7.5 7.5 0 1 0 9.5 9.5Z" />
      </svg>
    )
  }
  if (theme === 'light') {
    return (
      <svg width="18" height="18" viewBox="0 0 20 20" fill="none" aria-hidden="true" focusable="false">
        <circle cx="10" cy="10" r="4" fill="currentColor" />
        <g stroke="currentColor" strokeWidth="1.5" strokeLinecap="round">
          <path d="M10 2v2M10 16v2M18 10h-2M4 10H2M15.5 4.5l-1.4 1.4M5.9 14.1l-1.4 1.4M15.5 15.5l-1.4-1.4M5.9 5.9 4.5 4.5" />
        </g>
      </svg>
    )
  }
  return (
    <svg width="18" height="18" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" focusable="false">
      <path d="M10 2a8 8 0 1 0 0 16Z" />
      <path d="M10 2a8 8 0 0 1 0 16V2Z" fillOpacity="0.35" />
    </svg>
  )
}

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(() => getAppearance().theme)

  function cycle() {
    const next = NEXT[theme]
    const current = getAppearance()
    setAppearance({ ...current, theme: next })
    setTheme(next)
  }

  return (
    <button type="button" className="theme-toggle" aria-label={label(theme)} onClick={cycle}>
      <Glyph theme={theme} />
    </button>
  )
}
