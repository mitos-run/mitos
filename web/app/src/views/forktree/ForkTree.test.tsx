// Behavior test for ForkTree: renders the accessible table with one row per
// fork-tree node. Each node id links to /sandboxes (the list route). B2 will
// deep-link to the sandbox detail view once that route exists. The component
// is rendered directly (not via a route) using a minimal query + router
// harness so Link and useNavigate have the context they need. The /forks route
// is wired in Task 8; the route-level navigation assertion lives in the
// 'ForkTree route' describe block below.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent } from '@testing-library/react'
import { waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { ForkTree } from './ForkTree'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community',
  billing: false,
  signup: false,
  teams: true,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: true,
  ownership: 'self-hosted',
}

// Tiny router harness: a single root route renders ForkTree so Link and
// useNavigate get a real TanStack Router context without pulling in AppShell.
function renderForkTree() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const rootRoute = createRootRoute({ component: ForkTree })
  const router = createRouter({ routeTree: rootRoute.addChildren([]) })
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
}

const forkTreePayload = {
  org_id: 'o1',
  nodes: [
    { id: 'root', parent_id: '', phase: 'Running', private_dirty_bytes: 0, shared_bytes: 209715200 },
    { id: 'fork-a', parent_id: 'root', phase: 'Running', private_dirty_bytes: 3145728, shared_bytes: 209715200 },
  ],
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(
        new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/forktree')) {
      return Promise.resolve(
        new Response(JSON.stringify(forkTreePayload), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/sandboxes')) {
      return Promise.resolve(
        new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
  })
})

describe('ForkTree view', () => {
  it('renders every node in the accessible table', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    // Use getAllByRole: there are two data rows each containing the word "root"
    // (the id column for the root node, and the parent column for fork-a).
    const rows = screen.getAllByRole('row')
    // rows[0] is the header; rows[1] and rows[2] are the data rows.
    expect(rows.length).toBeGreaterThanOrEqual(3)
    // Verify by link presence: root row has a link named "root".
    expect(screen.getByRole('link', { name: 'root' })).toBeInTheDocument()
    // Verify fork-a row is present by its link.
    expect(screen.getByRole('link', { name: 'fork-a' })).toBeInTheDocument()
  })

  it('node id links resolve to the sandboxes list route (not a dead-end)', async () => {
    // Render the full app at /forks so navigation to /sandboxes actually
    // works. This proves the link resolves to a real route rather than the
    // not-found fallback. B2 will replace /sandboxes with /sandboxes/$id
    // once the sandbox detail route exists.
    await renderAt('/forks', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree nodes/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: /fork-a/i })
    // Confirm the href points at the list route, not the non-existent detail route.
    expect(link).toHaveAttribute('href', '/sandboxes')
    // Click the link and confirm the Sandboxes view appears (real route resolved).
    fireEvent.click(link)
    await waitFor(() => expect(screen.getByRole('heading', { name: /sandboxes/i })).toBeInTheDocument())
    // The not-found fallback must NOT appear.
    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument()
  })

  it('shows an error state when the fetch fails', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/forktree')) {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'internal server error' }), {
            status: 500,
            headers: { 'content-type': 'application/json' },
          }),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })
    renderForkTree()
    await waitFor(() =>
      expect(screen.getByText(/fork tree unavailable/i)).toBeInTheDocument(),
    )
    expect(screen.queryByText(/no forks yet/i)).not.toBeInTheDocument()
  })
})

describe('ForkTree route', () => {
  it('mounts at /forks and table is labelled Fork tree nodes', async () => {
    await renderAt('/forks', caps) // caps + fetch mock from the top of this file
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree nodes/i })).toBeInTheDocument())
    // Node id links point at the sandboxes list; B2 will add the detail route.
    const link = screen.getByRole('link', { name: /fork-a/i })
    expect(link).toHaveAttribute('href', '/sandboxes')
  })
})
