// content.test.ts: tests for the first-run content registry.
// TDD: this file was written before content.ts existed.

import { describe, it, expect } from 'vitest'
import { FIRST_RUN, getFirstRun } from './content'

const DASH_RE = /[–—]/

describe('getFirstRun', () => {
  it('returns the rollouts entry for slug "rollouts"', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.slug).toBe('rollouts')
  })

  it('rollouts snippet contains fork(', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippet).toContain('fork(')
  })

  it('getFirstRun(undefined) returns the generic default', () => {
    const entry = getFirstRun(undefined)
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippet.length).toBeGreaterThan(0)
  })

  it('getFirstRun("nope") falls back to the generic default', () => {
    const entry = getFirstRun('nope')
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippet.length).toBeGreaterThan(0)
  })
})

describe('FIRST_RUN entries: no em or en dashes', () => {
  it.each(FIRST_RUN)('$slug has no em/en dash in any text field', (entry) => {
    expect(DASH_RE.test(entry.title)).toBe(false)
    expect(DASH_RE.test(entry.lede)).toBe(false)
    expect(DASH_RE.test(entry.snippet)).toBe(false)
    expect(DASH_RE.test(entry.watchFor)).toBe(false)
  })
})
