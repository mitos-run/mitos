// Accessibility audit for ForkTree: the rendered HTML must pass axe-core with
// no violations. The component is rendered inside a minimal router context so
// Link components have the TanStack Router context they require. No rules are
// suppressed: any violation must be fixed in the component itself.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render } from '@testing-library/react'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { ForkTree } from './ForkTree'

expect.extend(matchers)

// Minimal router harness: ForkTree is the root component so Link + useNavigate
// work without importing AppShell or any application-level route tree.
function ForkTreeRouterHarness() {
  return <ForkTree />
}

function renderWithRouterAndQuery() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const rootRoute = createRootRoute({ component: ForkTreeRouterHarness })
  const router = createRouter({ routeTree: rootRoute.addChildren([]) })
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(
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
})

describe('ForkTree accessibility', () => {
  it('has no axe violations and exposes the data as a table', async () => {
    const { container } = renderWithRouterAndQuery()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
