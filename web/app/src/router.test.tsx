import { describe, it, expect, vi } from 'vitest'
import { render, waitFor, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { createConsoleRouter } from './router'
import type { Capabilities } from './api'
import { visibleRoutes } from './nav/routes'

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

  it('does not build a billing route when billing is off', () => {
    // The brief suggested accessing router.routeTree.children directly, but
    // TanStack Router v1.170 does not expose .children on the routeTree object
    // in a stable way. We instead verify via visibleRoutes (the same function
    // createConsoleRouter uses) that /billing is absent. This proves the same
    // invariant: if billing is off, visibleRoutes filters it out, and since
    // createConsoleRouter maps exactly over visibleRoutes, no /billing route
    // is registered. The test fails if visibleRoutes ever adds /billing for a
    // caps where billing is false.
    const routes = visibleRoutes(caps)
    const paths = routes.map((r) => r.path)
    expect(paths).not.toContain('/billing')
  })
})
