// firstKey.test.ts: unit tests for the one-time first-key sessionStorage helpers.
//
// TDD: written before firstKey.ts exists so the first run is a deliberate FAIL.
// Environment: jsdom (vitest config sets environment: 'jsdom'), so sessionStorage
// is available without any additional mocking.
//
// Security: the raw key is never logged or inspected in error paths; tests only
// assert return values and sessionStorage state, not log output.

import { describe, it, expect, beforeEach } from 'vitest'
import { peekFirstKey, takeFirstKey, maskKey } from './firstKey'

beforeEach(() => {
  sessionStorage.clear()
})

describe('peekFirstKey', () => {
  it('returns the stored key without removing it', () => {
    sessionStorage.setItem('mitos.firstKey', 'mk_live_a1b2c3d4e5')
    expect(peekFirstKey()).toBe('mk_live_a1b2c3d4e5')
    // Still present after peek
    expect(sessionStorage.getItem('mitos.firstKey')).toBe('mk_live_a1b2c3d4e5')
  })

  it('returns null when no key is stored', () => {
    expect(peekFirstKey()).toBeNull()
  })
})

describe('takeFirstKey', () => {
  it('returns the key and removes it so a subsequent peek is null', () => {
    sessionStorage.setItem('mitos.firstKey', 'mk_live_a1b2c3d4e5')
    const val = takeFirstKey()
    expect(val).toBe('mk_live_a1b2c3d4e5')
    expect(peekFirstKey()).toBeNull()
  })

  it('returns null when nothing is stored', () => {
    expect(takeFirstKey()).toBeNull()
  })
})

describe('maskKey', () => {
  it('starts with the first 12 characters of the key', () => {
    const masked = maskKey('mk_live_a1b2c3d4e5')
    expect(masked.startsWith('mk_live_a1b2')).toBe(true)
  })

  it('appends exactly 8 bullet characters after the prefix', () => {
    const masked = maskKey('mk_live_a1b2c3d4e5')
    expect(masked).toBe('mk_live_a1b2' + '•'.repeat(8))
  })

  it('does not contain any characters that follow index 12 in the raw key', () => {
    const key = 'mk_live_a1b2c3d4e5'
    const tail = key.slice(12) // 'c3d4e5'
    const masked = maskKey(key)
    expect(masked).not.toContain(tail)
  })
})
