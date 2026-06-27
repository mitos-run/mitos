import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { createPreAuthRouter } from './preauthRouter'

describe('pre-auth router', () => {
  it('renders the login route with a GitHub affordance', async () => {
    const router = createPreAuthRouter('/login')
    render(<RouterProvider router={router} />)
    expect(await screen.findByText(/Continue with GitHub/i)).toBeInTheDocument()
  })
})
