import { describe, it, expect, vi } from 'vitest'
import { render, waitFor, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { createConsoleRouter } from './router'
import type { Capabilities } from './api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('console router', () => {
  it('renders the sandboxes route', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
    const router = createConsoleRouter(caps)
    await router.navigate({ to: '/sandboxes' })
    render(<RouterProvider router={router} />)
    await waitFor(() => expect(screen.getByRole('heading', { name: /Sandboxes/i })).toBeInTheDocument())
  })

  it('omits /billing from the router when billing is off (negative control)', () => {
    // router.routesByPath is a Record<string, RouteObject> that TanStack Router
    // v1 populates during createRouter; it excludes the __root__ entry and
    // contains exactly the registered path routes. Asserting here (not on
    // visibleRoutes) means the test genuinely exercises createConsoleRouter.
    const router = createConsoleRouter(caps)
    expect(Object.keys(router.routesByPath)).not.toContain('/billing')
  })

  it('includes /billing in the router when billing is on (positive control)', () => {
    // Positive control: ensure the test would fail if gating were inverted or
    // createConsoleRouter ignored the capability. Build a router with billing
    // enabled and assert the route IS registered.
    const billingCaps: Capabilities = { ...caps, billing: true }
    const router = createConsoleRouter(billingCaps)
    expect(Object.keys(router.routesByPath)).toContain('/billing')
  })
})
