// Behavior tests for NewSandboxModal: template select (defaults to the first
// template), vcpu/mem selects offer exactly the static bounded options,
// submit posts the expected body, a server error surfaces as an inline
// message (not a silent failure), and Escape closes the modal.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent, waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { NewSandboxModal } from './NewSandboxModal'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const templates = [
  { name: 'python-3.12', org_id: 'o1', description: '', image: 'python:3.12', updated_at: '2026-01-01T00:00:00Z' },
  { name: 'node-22', org_id: 'o1', description: '', image: 'node:22', updated_at: '2026-01-01T00:00:00Z' },
]

let postedBody: unknown = null

function mockFetch(opts: { createStatus?: number; createBody?: unknown } = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    if (url.endsWith('/console/templates')) {
      return Promise.resolve(new Response(JSON.stringify({ templates }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/projects')) {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', projects: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/sandboxes') && method === 'POST') {
      postedBody = init?.body ? JSON.parse(String(init.body)) : null
      const status = opts.createStatus ?? 201
      const body = opts.createBody ?? { id: 'sbx-1', org_id: 'o1', template: 'python-3.12', node: '', phase: 'Pending', vcpus: 1, mem_bytes: 1 << 30, created_at: '2026-01-01T00:00:00Z' }
      return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  postedBody = null
})

describe('NewSandboxModal', () => {
  it('defaults the template select to the first template once loaded', async () => {
    mockFetch()
    wrap(<NewSandboxModal onClose={() => {}} />)
    await waitFor(() => expect(screen.getByLabelText(/template/i)).toHaveValue('python-3.12'))
  })

  it('labels the sizing selects as requests and explains sizing is not yet enforced per sandbox', async () => {
    mockFetch()
    wrap(<NewSandboxModal onClose={() => {}} />)
    await waitFor(() => expect(screen.getByLabelText(/template/i)).toBeInTheDocument())
    expect(screen.getByLabelText(/^requested vcpus$/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/^requested memory$/i)).toBeInTheDocument()
    expect(
      screen.getByText(
        /sizing is a request\. sandboxes currently run the template's resources; per-sandbox sizing is coming\./i,
      ),
    ).toBeInTheDocument()
  })

  it('offers exactly the static vcpu and memory options', async () => {
    mockFetch()
    wrap(<NewSandboxModal onClose={() => {}} />)
    await waitFor(() => expect(screen.getByLabelText(/template/i)).toBeInTheDocument())
    const vcpuOptions = screen.getAllByRole('option', { name: /^[124]$/ })
    expect(vcpuOptions.map((o) => (o as HTMLOptionElement).value)).toEqual(['1', '2', '4'])
    const memOptions = screen.getAllByRole('option', { name: /gib/i })
    expect(memOptions.map((o) => (o as HTMLOptionElement).value)).toEqual(['1', '2', '4', '8'])
  })

  it('submits the create request with the selected fields', async () => {
    mockFetch()
    const onCreated = vi.fn()
    const onClose = vi.fn()
    wrap(<NewSandboxModal onClose={onClose} onCreated={onCreated} />)
    await waitFor(() => expect(screen.getByLabelText(/template/i)).toHaveValue('python-3.12'))
    fireEvent.change(screen.getByLabelText(/vcpus/i), { target: { value: '2' } })
    fireEvent.change(screen.getByLabelText(/memory/i), { target: { value: '4' } })
    fireEvent.click(screen.getByRole('button', { name: /create sandbox/i }))
    await waitFor(() => expect(postedBody).toEqual({ template: 'python-3.12', vcpus: 2, mem_gib: 4, project_id: undefined }))
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith('sbx-1'))
    expect(onClose).toHaveBeenCalled()
  })

  it('shows an inline error and does not close when the server rejects', async () => {
    mockFetch({ createStatus: 400, createBody: { error: { cause: 'vcpus must be one of 1, 2, 4' } } })
    const onClose = vi.fn()
    wrap(<NewSandboxModal onClose={onClose} />)
    await waitFor(() => expect(screen.getByLabelText(/template/i)).toHaveValue('python-3.12'))
    fireEvent.click(screen.getByRole('button', { name: /create sandbox/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/vcpus must be one of 1, 2, 4/i))
    expect(onClose).not.toHaveBeenCalled()
  })

  it('calls onClose on Escape', async () => {
    mockFetch()
    const onClose = vi.fn()
    wrap(<NewSandboxModal onClose={onClose} />)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalled()
  })

  it('shows a message and no form when there are no templates', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/templates')) {
        return Promise.resolve(new Response(JSON.stringify({ templates: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    wrap(<NewSandboxModal onClose={() => {}} />)
    await waitFor(() => expect(screen.getByText(/no templates are available yet/i)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /create sandbox/i })).not.toBeInTheDocument()
  })
})
