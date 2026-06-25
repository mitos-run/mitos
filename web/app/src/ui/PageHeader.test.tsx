import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { PageHeader } from './PageHeader'

describe('PageHeader', () => {
  it('renders the title as the page h1 and an optional lede', () => {
    render(<PageHeader title="Sandboxes" lede="Running agents across your cluster." />)
    const h1 = screen.getByRole('heading', { level: 1, name: 'Sandboxes' })
    expect(h1).toBeInTheDocument()
    expect(screen.getByText('Running agents across your cluster.')).toBeInTheDocument()
  })

  it('omits the eyebrow, lede, and actions when not provided', () => {
    const { container } = render(<PageHeader title="Audit" />)
    expect(container.querySelector('.page-header-eyebrow')).toBeNull()
    expect(container.querySelector('.page-header-lede')).toBeNull()
    expect(container.querySelector('.page-header-actions')).toBeNull()
  })

  it('renders the eyebrow and an actions slot when provided', () => {
    render(<PageHeader eyebrow="GOVERN" title="Projects" actions={<button type="button">New project</button>} />)
    expect(screen.getByText('GOVERN')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New project' })).toBeInTheDocument()
  })
})
