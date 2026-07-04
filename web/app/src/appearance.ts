// Appearance preferences: reduced-motion toggle, layout density, and theme.
// Persisted to localStorage; applied immediately to document.documentElement.dataset
// so that CSS can react without a React re-render cycle.
// Theme: dark is the brand default (DEFAULTS.theme), so a first-time visitor with
// no stored preference gets data-theme="dark" regardless of OS preference.
// 'system' is a selectable opt-out that removes data-theme so the
// @media (prefers-color-scheme) default in @mitos/brand tokens.css decides;
// 'dark'/'light' pin data-theme explicitly.
// index.html applies the resolved theme (stored value, or 'dark' when nothing is
// stored / storage fails) before first paint; keep it in sync.
// Guard every localStorage call with try/catch for SSR or restricted contexts.
// setAppearance() notifies subscribe() listeners after persisting + applying,
// so every mounted control (TopBar's ThemeToggle, Settings' AppearanceTab)
// observes the same value instead of each holding its own stale copy. See
// useAppearance.ts for the React hook built on top of subscribe().

export type Density = 'comfortable' | 'compact'

export type Theme = 'system' | 'dark' | 'light'

export type Appearance = {
  reducedMotion: boolean
  density: Density
  theme: Theme
}

const STORAGE_KEY = 'mitos-appearance'

const DEFAULTS: Appearance = { reducedMotion: false, density: 'comfortable', theme: 'dark' }

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
  notify()
}

export function applyAppearanceOnLoad(): void {
  applyToDocument(getAppearance())
}

// --- Subscription registry ---
// Framework-free pub/sub so React (or anything else) can observe changes
// without appearance.ts depending on React. useAppearance.ts wraps this in a
// useSyncExternalStore-based hook.

type Listener = () => void

const listeners = new Set<Listener>()

export function subscribe(listener: Listener): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

function notify(): void {
  for (const listener of listeners) {
    listener()
  }
}
