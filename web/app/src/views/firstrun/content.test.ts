// content.test.ts: tests for the first-run content registry.
// TDD: this file was written before content.ts existed.

import { describe, it, expect } from 'vitest'
import { FIRST_RUN, RUNTIMES, getFirstRun } from './content'

const DASH_RE = /[–—]/

describe('RUNTIMES constant', () => {
  it('exports ids in order python, typescript, cli', () => {
    expect(RUNTIMES.map((r) => r.id)).toEqual(['python', 'typescript', 'cli'])
  })

  it('has a non-empty label for each runtime', () => {
    for (const r of RUNTIMES) {
      expect(r.label.length).toBeGreaterThan(0)
    }
  })
})

describe('getFirstRun', () => {
  it('returns the rollouts entry for slug "rollouts"', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.slug).toBe('rollouts')
  })

  it('rollouts snippets.python contains fork(', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.python).toContain('fork(')
  })

  it('rollouts snippets.typescript contains fork(', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.typescript).toContain('fork(')
  })

  it('rollouts snippets.cli contains mitos', () => {
    const entry = getFirstRun('rollouts')
    expect(entry.snippets.cli).toContain('mitos')
  })

  it('getFirstRun(undefined) returns the generic default with all runtimes non-empty', () => {
    const entry = getFirstRun(undefined)
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippets.python.length).toBeGreaterThan(0)
    expect(entry.snippets.typescript.length).toBeGreaterThan(0)
    expect(entry.snippets.cli.length).toBeGreaterThan(0)
  })

  it('getFirstRun("nope") falls back to the generic default with all runtimes non-empty', () => {
    const entry = getFirstRun('nope')
    expect(entry.slug).toBe('default')
    expect(entry.title.length).toBeGreaterThan(0)
    expect(entry.snippets.python.length).toBeGreaterThan(0)
    expect(entry.snippets.typescript.length).toBeGreaterThan(0)
    expect(entry.snippets.cli.length).toBeGreaterThan(0)
  })
})

describe('FIRST_RUN entries: no em or en dashes', () => {
  it.each(FIRST_RUN)('$slug has no em/en dash in any text field', (entry) => {
    expect(DASH_RE.test(entry.title)).toBe(false)
    expect(DASH_RE.test(entry.lede)).toBe(false)
    expect(DASH_RE.test(entry.snippets.python)).toBe(false)
    expect(DASH_RE.test(entry.snippets.typescript)).toBe(false)
    expect(DASH_RE.test(entry.snippets.cli)).toBe(false)
    expect(DASH_RE.test(entry.watchFor)).toBe(false)
  })
})
