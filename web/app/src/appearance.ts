// Appearance preferences: reduced-motion toggle + layout density.
// Persisted to localStorage; applied immediately to document.documentElement.dataset
// so that CSS can react without a React re-render cycle.
// Guard every localStorage call with try/catch for SSR or restricted contexts.

export type Density = 'comfortable' | 'compact'

export type Appearance = {
  reducedMotion: boolean
  density: Density
}

const STORAGE_KEY = 'mitos-appearance'

const DEFAULTS: Appearance = { reducedMotion: false, density: 'comfortable' }

export function getAppearance(): Appearance {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return { ...DEFAULTS }
    const parsed = JSON.parse(raw) as Partial<Appearance>
    return {
      reducedMotion: typeof parsed.reducedMotion === 'boolean' ? parsed.reducedMotion : DEFAULTS.reducedMotion,
      density: parsed.density === 'compact' || parsed.density === 'comfortable' ? parsed.density : DEFAULTS.density,
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
