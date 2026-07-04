// Behavior tests for the appearance module: round-trip localStorage persistence
// and document.documentElement.dataset side-effects.
import { describe, it, expect, beforeEach, vi } from 'vitest'
import { getAppearance, setAppearance, applyAppearanceOnLoad, subscribe } from './appearance'

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset['reduceMotion']
  delete document.documentElement.dataset['density']
  delete document.documentElement.dataset['theme']
})

describe('appearance', () => {
  it('round-trips setAppearance -> getAppearance for reducedMotion:true, density:compact', () => {
    setAppearance({ reducedMotion: true, density: 'compact', theme: 'system' })
    const got = getAppearance()
    expect(got.reducedMotion).toBe(true)
    expect(got.density).toBe('compact')
  })

  it('applies dataset.reduceMotion when reducedMotion is true', () => {
    setAppearance({ reducedMotion: true, density: 'comfortable', theme: 'system' })
    expect(document.documentElement.dataset['reduceMotion']).toBe('1')
  })

  it('removes dataset.reduceMotion when reducedMotion is false', () => {
    document.documentElement.dataset['reduceMotion'] = '1'
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'system' })
    expect(document.documentElement.dataset['reduceMotion']).toBeUndefined()
  })

  it('applies dataset.density', () => {
    setAppearance({ reducedMotion: false, density: 'compact', theme: 'system' })
    expect(document.documentElement.dataset['density']).toBe('compact')
  })

  it('returns defaults when nothing is stored, with theme defaulting to dark', () => {
    const got = getAppearance()
    expect(got.reducedMotion).toBe(false)
    expect(got.density).toBe('comfortable')
    expect(got.theme).toBe('dark')
  })

  it('applies dataset.theme = dark when nothing is stored (brand default, not OS preference)', () => {
    applyAppearanceOnLoad()
    expect(document.documentElement.dataset['theme']).toBe('dark')
  })

  it('round-trips theme through setAppearance -> getAppearance', () => {
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    expect(getAppearance().theme).toBe('light')
  })

  it('applies dataset.theme for an explicit light choice', () => {
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    expect(document.documentElement.dataset['theme']).toBe('light')
  })

  it('applies dataset.theme for an explicit dark choice', () => {
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'dark' })
    expect(document.documentElement.dataset['theme']).toBe('dark')
  })

  it('removes dataset.theme for system so prefers-color-scheme decides', () => {
    document.documentElement.dataset['theme'] = 'light'
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'system' })
    expect(document.documentElement.dataset['theme']).toBeUndefined()
  })

  it('falls back to the default (dark) theme when the stored value is invalid', () => {
    localStorage.setItem('mitos-appearance', JSON.stringify({ theme: 'sepia' }))
    expect(getAppearance().theme).toBe('dark')
  })

  it('returns the dark default when localStorage.getItem throws', () => {
    const spy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('storage unavailable')
    })
    try {
      const got = getAppearance()
      expect(got).toEqual({ reducedMotion: false, density: 'comfortable', theme: 'dark' })
    } finally {
      spy.mockRestore()
    }
  })
})

describe('appearance subscribe', () => {
  it('notifies subscribers after setAppearance persists and applies', () => {
    const listener = vi.fn()
    subscribe(listener)
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    expect(listener).toHaveBeenCalledTimes(1)
    expect(document.documentElement.dataset['theme']).toBe('light')
  })

  it('stops notifying once unsubscribed', () => {
    const listener = vi.fn()
    const unsubscribe = subscribe(listener)
    unsubscribe()
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    expect(listener).not.toHaveBeenCalled()
  })
})
