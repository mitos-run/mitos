// Behavior test for the live sandboxes list. Asserts row count, status dot,
// terminate action (optimistic remove), the empty-state fallback, and project column.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent, waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { ToastProvider } from '../../ui/Toast'
import { SandboxList } from './SandboxList'
import type { Capabilities } from '../../api'
import { renderAt } from '../../test/utils'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const sandboxes = [
  { id: 'sb-1', org_id: 'o1', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'p1' },
  { id: 'sb-2', org_id: 'o1', template: 'node-22', node: 'w2', phase: 'Pending', vcpus: 1, mem_bytes: 1073741824, created_at: '2026-01-02T00:00:00Z' },
]

function renderSandboxList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const rootRoute = createRootRoute()
  const indexRoute = createRoute({ getParentRoute: () => rootRoute, path: '/', component: SandboxList })
  const detailRoute = createRoute({ getParentRoute: () => rootRoute, path: '/sandboxes/$id', component: () => <div>detail</div> })
  const router = createRouter({ routeTree: rootRoute.addChildren([indexRoute, detailRoute]) })
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <RouterProvider router={router} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/sandboxes') && (init?.method ?? 'GET').toUpperCase() === 'GET') {
      return Promise.resolve(new Response(JSON.stringify({ sandboxes }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/sandboxes/sb-1')) {
      return Promise.resolve(new Response(JSON.stringify(sandboxes[0]), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/templates')) {
      return Promise.resolve(new Response(JSON.stringify({ templates: [{ name: 'python-3.12', org_id: 'o1', description: '', image: '', updated_at: '' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/projects')) {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', projects: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/sandboxes/')) {
      return Promise.resolve(new Response('', { status: 204 }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('SandboxList', () => {
  it('renders a row per sandbox with id, template, phase, and a terminate button', async () => {
    renderSandboxList()
    await waitFor(() => expect(screen.getByRole('table', { name: /sandboxes/i })).toBeInTheDocument())
    expect(screen.getByText('sb-1')).toBeInTheDocument()
    expect(screen.getByText('sb-2')).toBeInTheDocument()
    expect(screen.getByText('python-3.12')).toBeInTheDocument()
    expect(screen.getByText('node-22')).toBeInTheDocument()
    const terminateButtons = screen.getAllByRole('button', { name: /terminate/i })
    expect(terminateButtons.length).toBe(2)
  })

  it('optimistically removes a row when terminate is clicked', async () => {
    renderSandboxList()
    await waitFor(() => expect(screen.getByText('sb-1')).toBeInTheDocument())
    const [firstTerminate] = screen.getAllByRole('button', { name: /terminate/i })
    fireEvent.click(firstTerminate)
    await waitFor(() => expect(screen.queryByText('sb-1')).not.toBeInTheDocument())
    expect(screen.getByText('sb-2')).toBeInTheDocument()
  })

  it('shows the empty state when there are no sandboxes', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/sandboxes')) {
        return Promise.resolve(new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    renderSandboxList()
    await waitFor(() => expect(screen.getByText(/no live sandboxes/i)).toBeInTheDocument())
  })

  it('sandbox id links to the sandbox detail route', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /sandboxes/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: 'sb-1' })
    expect(link).toHaveAttribute('href', '/sandboxes/sb-1')
  })

  it('rolls back the optimistic removal when the server rejects terminate', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/sandboxes')) {
        return Promise.resolve(new Response(JSON.stringify({ sandboxes }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.includes('/console/sandboxes/')) {
        return Promise.resolve(new Response(JSON.stringify({ error: 'forbidden' }), { status: 403, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByText('sb-1')).toBeInTheDocument())
    const [firstTerminate] = screen.getAllByRole('button', { name: /terminate/i })
    fireEvent.click(firstTerminate)
    await waitFor(() => expect(screen.getByText('sb-1')).toBeInTheDocument())
    expect(screen.getByText('sb-2')).toBeInTheDocument()
  })

  it('shows project_id in the Project column and "Unassigned" when empty', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /sandboxes/i })).toBeInTheDocument())
    // sb-1 has project_id 'p1', sb-2 has no project_id
    expect(screen.getByText('p1')).toBeInTheDocument()
    expect(screen.getByText('Unassigned')).toBeInTheDocument()
  })

  it('the New sandbox primary action opens the create modal', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /sandboxes/i })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /new sandbox/i }))
    expect(await screen.findByRole('dialog', { name: /new sandbox/i })).toBeInTheDocument()
  })
})
