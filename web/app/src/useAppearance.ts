// React hook over the shared appearance store (appearance.ts). appearance.ts
// stays framework-free (it's read by index.html's pre-paint script too), so
// the useSyncExternalStore wiring lives here instead.
//
// getSnapshot must return a *cached* object whose identity only changes when
// the underlying value actually changes; otherwise useSyncExternalStore sees
// a new object on every call and re-renders in an infinite loop.
import { useSyncExternalStore } from 'react'
import { getAppearance, subscribe, type Appearance } from './appearance'

let cached: Appearance = getAppearance()

function getSnapshot(): Appearance {
  const next = getAppearance()
  if (
    next.reducedMotion !== cached.reducedMotion ||
    next.density !== cached.density ||
    next.theme !== cached.theme
  ) {
    cached = next
  }
  return cached
}

export function useAppearance(): Appearance {
  return useSyncExternalStore(subscribe, getSnapshot)
}
