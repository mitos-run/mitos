// Placeholder must be a teaching state: plain language about what the page
// will do, an honest "not available yet", and a concrete today alternative.
// It must never leak internal endpoints or roadmap phases into user copy.
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Placeholder } from './Placeholder'
import { ROUTES } from '../nav/routes'

describe('Placeholder', () => {
  it('renders the title, description, and not-available-yet line', () => {
    render(
      <Placeholder
        title="Workspaces"
        description="Browse workspaces and their revisions."
        today={<>Use the CLI: <code>mitos ws ls</code>.</>}
      />,
    )
    expect(screen.getByRole('heading', { name: 'Workspaces' })).toBeInTheDocument()
    expect(screen.getByText(/Browse workspaces/)).toBeInTheDocument()
    expect(screen.getByText(/not available yet/i)).toBeInTheDocument()
    expect(screen.getByText('mitos ws ls')).toBeInTheDocument()
  })

  it('the Workspaces route teaches the CLI alternative and leaks no internals', () => {
    const route = ROUTES.find((r) => r.path === '/workspaces')!
    const { container } = render(route.element())
    // Concrete next action: the CLI workspace surface.
    expect(screen.getByText(/mitos ws create/)).toBeInTheDocument()
    expect(screen.getByText('mitos ws ls')).toBeInTheDocument()
    // No internal jargon in user-facing copy.
    const text = container.textContent ?? ''
    expect(text).not.toMatch(/BFF/i)
    expect(text).not.toMatch(/\/console\//)
    expect(text).not.toMatch(/\bB2\b/)
    expect(text).not.toMatch(/endpoint/i)
  })
})
