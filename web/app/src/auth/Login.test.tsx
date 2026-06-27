import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
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
})
