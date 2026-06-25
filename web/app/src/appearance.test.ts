// Behavior tests for the appearance module: round-trip localStorage persistence
// and document.documentElement.dataset side-effects.
import { describe, it, expect, beforeEach } from 'vitest'
import { getAppearance, setAppearance } from './appearance'

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset['reduceMotion']
  delete document.documentElement.dataset['density']
})

describe('appearance', () => {
  it('round-trips setAppearance -> getAppearance for reducedMotion:true, density:compact', () => {
    setAppearance({ reducedMotion: true, density: 'compact' })
    const got = getAppearance()
    expect(got.reducedMotion).toBe(true)
    expect(got.density).toBe('compact')
  })

  it('applies dataset.reduceMotion when reducedMotion is true', () => {
    setAppearance({ reducedMotion: true, density: 'comfortable' })
    expect(document.documentElement.dataset['reduceMotion']).toBe('1')
  })

  it('removes dataset.reduceMotion when reducedMotion is false', () => {
    document.documentElement.dataset['reduceMotion'] = '1'
    setAppearance({ reducedMotion: false, density: 'comfortable' })
    expect(document.documentElement.dataset['reduceMotion']).toBeUndefined()
  })

  it('applies dataset.density', () => {
    setAppearance({ reducedMotion: false, density: 'compact' })
    expect(document.documentElement.dataset['density']).toBe('compact')
  })

  it('returns defaults when nothing is stored', () => {
    const got = getAppearance()
    expect(got.reducedMotion).toBe(false)
    expect(got.density).toBe('comfortable')
  })
})
