// Appearance preferences: reduced-motion toggle, layout density, and theme.
// Persisted to localStorage; applied immediately to document.documentElement.dataset
// so that CSS can react without a React re-render cycle.
// Theme: 'system' removes data-theme so the @media (prefers-color-scheme) default
// in @mitos/brand tokens.css decides; 'dark'/'light' pin data-theme explicitly.
// index.html applies a stored explicit theme before first paint; keep it in sync.
// Guard every localStorage call with try/catch for SSR or restricted contexts.

export type Density = 'comfortable' | 'compact'

export type Theme = 'system' | 'dark' | 'light'

export type Appearance = {
  reducedMotion: boolean
  density: Density
  theme: Theme
}

const STORAGE_KEY = 'mitos-appearance'

const DEFAULTS: Appearance = { reducedMotion: false, density: 'comfortable', theme: 'system' }

export function getAppearance(): Appearance {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return { ...DEFAULTS }
    const parsed = JSON.parse(raw) as Partial<Appearance>
    return {
      reducedMotion: typeof parsed.reducedMotion === 'boolean' ? parsed.reducedMotion : DEFAULTS.reducedMotion,
      density: parsed.density === 'compact' || parsed.density === 'comfortable' ? parsed.density : DEFAULTS.density,
      theme: parsed.theme === 'dark' || parsed.theme === 'light' || parsed.theme === 'system' ? parsed.theme : DEFAULTS.theme,
    }
  } catch {
    return { ...DEFAULTS }
  }
}

function applyToDocument(a: Appearance): void {
  try {
    if (a.reducedMotion) {
      document.documentElement.dataset['reduceMotion'] = '1'
    } else {
      delete document.documentElement.dataset['reduceMotion']
    }
    document.documentElement.dataset['density'] = a.density
    if (a.theme === 'system') {
      delete document.documentElement.dataset['theme']
    } else {
      document.documentElement.dataset['theme'] = a.theme
    }
  } catch {
    // Non-browser context: no-op
  }
}

export function setAppearance(a: Appearance): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(a))
  } catch {
    // Storage unavailable: still apply to document
  }
  applyToDocument(a)
}

export function applyAppearanceOnLoad(): void {
  applyToDocument(getAppearance())
}
