// Behavior tests for useAppearance: the shared-store React hook that keeps
// every consumer (TopBar's ThemeToggle, Settings' AppearanceTab, etc.) in
// sync with the single localStorage-backed appearance value, instead of each
// holding its own stale useState copy.
import { describe, it, expect, beforeEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useAppearance } from './useAppearance'
import { setAppearance } from './appearance'

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset['reduceMotion']
  delete document.documentElement.dataset['density']
  delete document.documentElement.dataset['theme']
})

describe('useAppearance', () => {
  it('reads the current appearance on mount', () => {
    const { result } = renderHook(() => useAppearance())
    expect(result.current).toEqual({ reducedMotion: false, density: 'comfortable', theme: 'dark' })
  })

  it('updates when setAppearance is called externally (e.g. from another mounted control)', () => {
    const { result } = renderHook(() => useAppearance())
    act(() => {
      setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    })
    expect(result.current.theme).toBe('light')
  })

  it('keeps snapshot object identity stable across re-renders when nothing changed', () => {
    const { result, rerender } = renderHook(() => useAppearance())
    const first = result.current
    rerender()
    expect(result.current).toBe(first)
  })

  it('shares updates across two independently mounted consumers, fixing the desync between TopBar and Settings', () => {
    const a = renderHook(() => useAppearance())
    const b = renderHook(() => useAppearance())

    act(() => {
      setAppearance({ reducedMotion: true, density: 'compact', theme: 'system' })
    })

    expect(a.result.current).toEqual({ reducedMotion: true, density: 'compact', theme: 'system' })
    expect(b.result.current).toEqual({ reducedMotion: true, density: 'compact', theme: 'system' })
  })
})
