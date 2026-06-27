import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { Login } from './Login'

describe('Login page', () => {
  it('offers GitHub and Google with connector hints', () => {
    render(<Login />)
    expect(screen.getByRole('link', { name: /Continue with GitHub/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=github',
    )
    expect(screen.getByRole('link', { name: /Continue with Google/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=google',
    )
  })

  it('renders an email input', () => {
    render(<Login />)
    expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
  })

  it('renders a signup affordance', () => {
    render(<Login />)
    expect(screen.getByRole('button', { name: /sign up with email/i })).toBeInTheDocument()
  })

  it('propagates ?next= to provider connector hrefs', () => {
    render(<Login next="/dashboard" />)
    expect(screen.getByRole('link', { name: /Continue with GitHub/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=github&next=%2Fdashboard',
    )
    expect(screen.getByRole('link', { name: /Continue with Google/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=google&next=%2Fdashboard',
    )
  })

  it('propagates next= on email form submission', () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    render(<Login next="/dashboard" />)
    fireEvent.submit(screen.getByRole('form', { name: /continue with email/i }))
    expect(assign).toHaveBeenCalledWith(expect.stringContaining('next=%2Fdashboard'))
    vi.unstubAllGlobals()
  })
})
