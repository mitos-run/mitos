import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { fmtRelative, fmtAbsolute } from './dates'

describe('fmtRelative', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-06-25T12:00:00Z'))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders "just now" for timestamps under a minute old', () => {
    expect(fmtRelative('2026-06-25T11:59:45Z')).toBe('just now')
  })

  it('renders minutes for under an hour', () => {
    expect(fmtRelative('2026-06-25T11:45:00Z')).toBe('15m ago')
  })

  it('renders hours for under a day', () => {
    expect(fmtRelative('2026-06-25T09:00:00Z')).toBe('3h ago')
  })

  it('renders days for under a week', () => {
    expect(fmtRelative('2026-06-23T12:00:00Z')).toBe('2d ago')
  })

  it('falls back to an absolute date at 7 days or older', () => {
    const iso = '2026-06-10T12:00:00Z'
    const result = fmtRelative(iso)
    expect(result).not.toMatch(/ago$/)
    expect(result).toBe(fmtAbsolute(iso))
  })

  it('returns the raw string for an unparseable date', () => {
    expect(fmtRelative('not-a-date')).toBe('not-a-date')
  })
})

describe('fmtAbsolute', () => {
  it('formats using the given locale and timezone', () => {
    // 23:30 UTC on 2026-06-25 is 19:30 the same day in New York (EDT, UTC-4).
    const result = fmtAbsolute('2026-06-25T23:30:00Z', 'en-US', 'America/New_York')
    expect(result).toContain('2026')
    expect(result).toMatch(/7:30|19:30/i)
  })

  it('differs between two distinct timezones for the same instant', () => {
    const iso = '2026-06-25T23:30:00Z'
    const ny = fmtAbsolute(iso, 'en-US', 'America/New_York')
    const tokyo = fmtAbsolute(iso, 'en-US', 'Asia/Tokyo')
    expect(ny).not.toBe(tokyo)
  })

  it('falls back to browser defaults when locale/timezone are omitted', () => {
    expect(() => fmtAbsolute('2026-06-25T23:30:00Z')).not.toThrow()
  })

  it('returns the raw string for an unparseable date', () => {
    expect(fmtAbsolute('not-a-date')).toBe('not-a-date')
  })
})
