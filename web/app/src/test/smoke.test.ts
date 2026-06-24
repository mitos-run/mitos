import { describe, it, expect } from 'vitest'

describe('test harness', () => {
  it('runs and has jsdom', () => {
    const el = document.createElement('div')
    el.textContent = 'mitos'
    expect(el.textContent).toBe('mitos')
  })
})
