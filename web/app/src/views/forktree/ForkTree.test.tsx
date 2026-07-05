// Behavior test for ForkTree: renders the accessible table with one row per
// fork-tree node. Each node id deep-links to its sandbox detail view at
// /sandboxes/$id. The component is rendered directly (not via a route) using
// a minimal query + router harness so Link and useNavigate have the context
// they need. The /forks route is wired in Task 8; the route-level navigation
// assertion lives in the 'ForkTree route' describe block below.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent } from '@testing-library/react'
import { waitFor, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { ForkTree } from './ForkTree'
import { renderAt } from '../../test/utils'
import { ToastProvider } from '../../ui/Toast'
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
      <ToastProvider>
        <RouterProvider router={router} />
      </ToastProvider>
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

const sandboxForkA = {
  id: 'fork-a', org_id: 'o1', template: 'python-3.12', node: 'w1',
  phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z',
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
    // Sandbox detail fetch for fork-a (used by SandboxDetail when navigating to /sandboxes/fork-a).
    if (url.includes('/console/sandboxes/fork-a')) {
      return Promise.resolve(
        new Response(JSON.stringify(sandboxForkA), { status: 200, headers: { 'content-type': 'application/json' } }),
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

  it('node id links deep-link to the sandbox detail route (not a dead-end)', async () => {
    // Render the full app at /forks so navigation to /sandboxes/fork-a actually
    // works. This proves the link resolves to a real detail route rather than
    // the not-found fallback.
    await renderAt('/forks', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree nodes/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: /fork-a/i })
    // Confirm the href points at the sandbox detail route, not the list route.
    expect(link).toHaveAttribute('href', '/sandboxes/fork-a')
    // Click the link and confirm the sandbox detail view appears (real route resolved).
    fireEvent.click(link)
    await waitFor(() => expect(screen.getByRole('heading', { name: /fork-a/i })).toBeInTheDocument())
    // The not-found fallback must NOT appear.
    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument()
  })

  // Touch: each node's small visible circle (radius as low as MIN_R=10, a
  // 20px diameter) sits behind a larger invisible hit-target circle so a
  // tap on a phone/tablet has a comfortable >=44px-diameter target even
  // though the visible node stays small. The SVG itself stays aria-hidden
  // (the table is the a11y source of truth), so this only affects pointer
  // hit-testing, not the accessibility tree.
  it('gives every node an invisible hit-target circle at least 22px in radius', async () => {
    const { container } = renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const hitCircles = Array.from(container.querySelectorAll('circle[fill="transparent"]'))
    expect(hitCircles.length).toBe(2)
    hitCircles.forEach((c) => {
      expect(Number(c.getAttribute('r'))).toBeGreaterThanOrEqual(22)
    })
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
    // Node id links deep-link to the sandbox detail view.
    const link = screen.getByRole('link', { name: /fork-a/i })
    expect(link).toHaveAttribute('href', '/sandboxes/fork-a')
  })
})

describe('ForkTree node detail panel', () => {
  it('opens a side panel with id, phase, and byte fields when a row Details button is activated (keyboard reachable)', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const detailsButtons = screen.getAllByRole('button', { name: /view details/i })
    expect(detailsButtons.length).toBe(2)
    // A real <button> is keyboard-focusable and Enter/Space just works; a
    // click stands in for that activation here.
    fireEvent.click(screen.getByRole('button', { name: /view details for fork-a/i }))
    const panel = screen.getByRole('region', { name: /details for sandbox fork-a/i })
    expect(panel).toHaveTextContent('Running')
    expect(within(panel).getByRole('link', { name: /open/i })).toHaveAttribute('href', '/sandboxes/fork-a')
    expect(within(panel).getByRole('button', { name: /^fork/i })).toBeInTheDocument()
    expect(within(panel).getByRole('button', { name: /terminate/i })).toBeInTheDocument()
  })

  it('closes the panel via the Close button', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /view details for root/i }))
    expect(screen.getByRole('region', { name: /details for sandbox root/i })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /close details/i }))
    expect(screen.queryByRole('region', { name: /details for sandbox root/i })).not.toBeInTheDocument()
  })

  // Keyboard/screen-reader users get no signal a panel opened unless focus
  // actually moves into it; opening via the table's "Details" button must
  // land focus on the panel's heading.
  it('moves focus into the panel (onto its heading) when opened via the Details button', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /view details for fork-a/i }))
    const panel = screen.getByRole('region', { name: /details for sandbox fork-a/i })
    const heading = within(panel).getByRole('heading', { name: 'fork-a' })
    await waitFor(() => expect(document.activeElement).toBe(heading))
  })

  // Switching directly from one node's open panel to a different node's
  // (without closing in between) must move focus/announcement again. Uses
  // userEvent (not fireEvent) because real browsers move focus to a button
  // on click; fireEvent.click does not simulate that, which would mask the
  // regression this guards (the panel component is keyed by node.id so
  // React remounts it and reruns the heading focus effect, rather than
  // reusing the same instance and leaving focus on the just-clicked button).
  it('moves focus to the new heading when a different row Details button is activated while the panel is already open', async () => {
    const user = userEvent.setup()
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: /view details for root/i }))
    const rootPanel = screen.getByRole('region', { name: /details for sandbox root/i })
    const rootHeading = within(rootPanel).getByRole('heading', { name: 'root' })
    await waitFor(() => expect(document.activeElement).toBe(rootHeading))

    await user.click(screen.getByRole('button', { name: /view details for fork-a/i }))
    expect(screen.queryByRole('region', { name: /details for sandbox root/i })).not.toBeInTheDocument()
    const forkPanel = screen.getByRole('region', { name: /details for sandbox fork-a/i })
    const forkHeading = within(forkPanel).getByRole('heading', { name: 'fork-a' })
    await waitFor(() => expect(document.activeElement).toBe(forkHeading))
  })

  // Closing the panel must return focus to the button that opened it, not
  // silently drop it back to <body>.
  it('returns focus to the triggering Details button when the panel is closed', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const trigger = screen.getByRole('button', { name: /view details for root/i })
    fireEvent.click(trigger)
    expect(screen.getByRole('region', { name: /details for sandbox root/i })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /close details/i }))
    expect(document.activeElement).toBe(trigger)
  })

  // Mobile: the panel carries fork-node-panel so base.css's <=480px media
  // query pins it to the bottom of the viewport as a sheet, instead of
  // opening inline above an already-tall page.
  it('carries the fork-node-panel class for the mobile bottom-sheet treatment', async () => {
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /view details for root/i }))
    const panel = screen.getByRole('region', { name: /details for sandbox root/i })
    expect(panel.className).toContain('fork-node-panel')
  })

  it('Fork posts to the fork endpoint with the chosen count', async () => {
    let forkBody: unknown = null
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/forktree')) {
        return Promise.resolve(new Response(JSON.stringify(forkTreePayload), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.includes('/fork') && method === 'POST') {
        forkBody = init?.body ? JSON.parse(String(init.body)) : null
        return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', source: 'fork-a', ids: ['fork-a-fork-1', 'fork-a-fork-2'] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    renderForkTree()
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /view details for fork-a/i }))
    const countInput = screen.getByLabelText(/^fork$/i)
    fireEvent.change(countInput, { target: { value: '2' } })
    fireEvent.click(screen.getByRole('button', { name: /^fork 2/i }))
    await waitFor(() => expect(forkBody).toEqual({ count: 2 }))
  })
})
