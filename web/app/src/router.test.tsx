import { describe, it, expect, vi } from 'vitest'
import { render, waitFor, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createConsoleRouter } from './router'
import { ToastProvider } from './ui/Toast'
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
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
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

  it('resolves / to the Overview view when proof is off (Overview is always built)', async () => {
    // Overview is a real operational home now, so the '/' route is always built
    // regardless of the proof capability. Navigating to '/' must show Overview.
    const noproofCaps: Capabilities = { ...caps, proof: false }
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(noproofCaps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/sandboxes')) {
        return Promise.resolve(new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/instruments')) {
        return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', activate_p50_ms: 0, activate_p99_ms: 0, forks_served: 0, cow_savings_bytes: 0, marginal_bytes_per_fork: 0 }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/audit')) {
        return Promise.resolve(new Response(JSON.stringify({ events: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const router = createConsoleRouter(noproofCaps)
    await router.navigate({ to: '/' })
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <RouterProvider router={router} />
        </ToastProvider>
      </QueryClientProvider>,
    )
    // Must show the Overview heading, never "Not Found".
    await waitFor(() => expect(screen.getByRole('heading', { name: /Overview/i })).toBeInTheDocument())
    expect(screen.queryByText('Not Found')).not.toBeInTheDocument()
  })
})
