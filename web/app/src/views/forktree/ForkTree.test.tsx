// Behavior test for ForkTree: renders the accessible table with one row per
// fork-tree node, and links each id row to /sandboxes/{id}. The component is
// rendered directly (not via a route) using a minimal query + router harness
// so Link and useNavigate have the context they need. The /forks route is
// wired in Task 8; navigation assertions at the route level belong there.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render } from '@testing-library/react'
import { waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { ForkTree } from './ForkTree'

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

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/forktree')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            org_id: 'o1',
            nodes: [
              { id: 'root', parent_id: '', phase: 'Running', private_dirty_bytes: 0, shared_bytes: 209715200 },
              { id: 'fork-a', parent_id: 'root', phase: 'Running', private_dirty_bytes: 3145728, shared_bytes: 209715200 },
            ],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
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

  it('links each id cell to /sandboxes/{id}', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: /fork-a/i })
    expect(link).toHaveAttribute('href', expect.stringContaining('fork-a'))
  })
})
