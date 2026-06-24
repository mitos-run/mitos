import { describe, it, expect, vi } from 'vitest'
import { render, waitFor, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createConsoleRouter } from './router'
import type { Capabilities } from './api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('console router', () => {
  it('renders the sandboxes route', async () => {
    // AppShell is now the root layout and calls useCapabilities(), so the render
    // needs a QueryClientProvider and a capabilities fetch mock. The test intent
    // is unchanged: assert that /sandboxes produces the Sandboxes heading.
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const router = createConsoleRouter(caps)
    await router.navigate({ to: '/sandboxes' })
    render(
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    )
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
