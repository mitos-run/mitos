// Behavior tests for ThemeToggle: cycling order, persistence via
// setAppearance, and the dynamic aria-label announcing current + next theme.
import { describe, it, expect, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ThemeToggle } from './ThemeToggle'
import { getAppearance, setAppearance } from '../appearance'

beforeEach(() => {
  localStorage.clear()
  delete document.documentElement.dataset['theme']
})

describe('ThemeToggle', () => {
  it('renders dark as the initial state with a label announcing the next theme', () => {
    render(<ThemeToggle />)
    expect(screen.getByRole('button', { name: 'Theme: dark. Activate for light.' })).toBeInTheDocument()
  })

  it('cycles dark -> light -> system -> dark on repeated clicks, persisting each step', async () => {
    render(<ThemeToggle />)
    const btn = screen.getByRole('button', { name: /theme:/i })

    await userEvent.click(btn)
    expect(getAppearance().theme).toBe('light')
    expect(screen.getByRole('button', { name: 'Theme: light. Activate for system.' })).toBeInTheDocument()

    await userEvent.click(btn)
    expect(getAppearance().theme).toBe('system')
    expect(screen.getByRole('button', { name: 'Theme: system. Activate for dark.' })).toBeInTheDocument()

    await userEvent.click(btn)
    expect(getAppearance().theme).toBe('dark')
    expect(screen.getByRole('button', { name: 'Theme: dark. Activate for light.' })).toBeInTheDocument()
  })

  it('preserves reducedMotion and density when persisting a new theme', async () => {
    setAppearance({ reducedMotion: true, density: 'compact', theme: 'dark' })
    render(<ThemeToggle />)
    await userEvent.click(screen.getByRole('button', { name: /theme:/i }))
    const got = getAppearance()
    expect(got.theme).toBe('light')
    expect(got.reducedMotion).toBe(true)
    expect(got.density).toBe('compact')
  })

  it('reads the stored theme on mount instead of always starting at dark', () => {
    setAppearance({ reducedMotion: false, density: 'comfortable', theme: 'light' })
    render(<ThemeToggle />)
    expect(screen.getByRole('button', { name: 'Theme: light. Activate for system.' })).toBeInTheDocument()
  })

  it('applies data-theme to the document after a click', async () => {
    render(<ThemeToggle />)
    await userEvent.click(screen.getByRole('button', { name: /theme:/i }))
    expect(document.documentElement.dataset['theme']).toBe('light')
  })
})
