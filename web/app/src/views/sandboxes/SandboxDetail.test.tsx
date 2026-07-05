import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, fireEvent, waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const projects = [
  { id: 'proj-a', org_id: 'o', name: 'Alpha', description: '', created_at: '2026-01-01T00:00:00Z' },
  { id: 'proj-b', org_id: 'o', name: 'Beta', description: '', created_at: '2026-01-02T00:00:00Z' },
]

let putCalls: { url: string; body: unknown }[] = []

beforeEach(() => {
  putCalls = []
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.includes('/console/sandboxes/s1/logs')) return Promise.resolve(new Response('boot ok\nlistening', { status: 200 }))
    if (url.includes('/console/sandboxes/s1/project') && method === 'PUT') {
      const body = init?.body ? JSON.parse(String(init.body)) as unknown : undefined
      putCalls.push({ url, body })
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/forktree')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', nodes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

class MockEventSource {
  static instances: MockEventSource[] = []
  url: string
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
}

describe('SandboxDetail', () => {
  it('renders the sandbox overview and switches to the Logs tab', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    expect(screen.getByText('python-3.12')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: /logs/i }))
    await waitFor(() => expect(screen.getByText(/listening/)).toBeInTheDocument())
  })

  it('header Fork button posts to the fork endpoint', async () => {
    let forkBody: unknown = null
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.includes('/console/sandboxes/s1/fork') && method === 'POST') {
        forkBody = init?.body ? JSON.parse(String(init.body)) : null
        return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', source: 's1', ids: ['s1-fork-1'] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /^fork$/i }))
    await waitFor(() => expect(forkBody).toEqual({ count: 1 }))
  })

  it('header Terminate button deletes the sandbox and navigates back to the list', async () => {
    let terminated = false
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.endsWith('/console/sandboxes/s1') && method === 'DELETE') {
        terminated = true
        return Promise.resolve(new Response(null, { status: 200 }))
      }
      if (url.endsWith('/console/sandboxes') && method === 'GET') return Promise.resolve(new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /terminate s1/i }))
    await waitFor(() => expect(terminated).toBe(true))
    // Navigated back to the sandboxes list (an empty one, per this mock).
    await waitFor(() => expect(screen.getByRole('heading', { name: /^sandboxes$/i })).toBeInTheDocument())
  })

  it('Terminal tab shows the RunCommand panel with honest PTY copy and runs a command', async () => {
    let execBody: unknown = null
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.includes('/console/sandboxes/s1/exec') && method === 'POST') {
        execBody = init?.body ? JSON.parse(String(init.body)) : null
        return Promise.resolve(new Response(JSON.stringify({ stdout: 'hi\n', stderr: '', exit_code: 0 }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /terminal/i }))
    expect(screen.getByText(/full pty is coming/i)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText(/command/i), { target: { value: 'echo hi' } })
    fireEvent.click(screen.getByRole('button', { name: /^run$/i }))
    await waitFor(() => expect(execBody).toEqual({ cmd: 'echo hi', timeout_s: 0 }))
    await waitFor(() => expect(screen.getByText('hi')).toBeInTheDocument())
    expect(screen.getByText(/exit code 0/i)).toBeInTheDocument()
  })

  it('Logs tab Live toggle switches to the SSE stream', async () => {
    vi.stubGlobal('EventSource', MockEventSource)
    MockEventSource.instances = []
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    await userEvent.click(screen.getByRole('tab', { name: /logs/i }))
    await waitFor(() => expect(screen.getByText(/listening/)).toBeInTheDocument())
    fireEvent.click(screen.getByLabelText(/live logs/i))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    expect(MockEventSource.instances[0].url).toBe('/console/sandboxes/s1/logs/stream')
    act(() => {
      MockEventSource.instances[0].onopen?.()
      MockEventSource.instances[0].onmessage?.({ data: 'live line 1' })
    })
    await waitFor(() => expect(screen.getByText('live line 1')).toBeInTheDocument())
    vi.unstubAllGlobals()
  })

  it('renders a Project select with the current project pre-selected', async () => {
    await renderAt('/sandboxes/s1', caps)
    // wait until projects have loaded and the correct option is selected
    await waitFor(() => {
      const select = screen.getByLabelText(/project/i) as HTMLSelectElement
      expect(select.value).toBe('proj-a')
    })
  })

  it('includes an Unassigned option and options for each org project', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    expect(screen.getByRole('option', { name: /unassigned/i })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'Beta' })).toBeInTheDocument()
  })

  it('calls PUT /console/sandboxes/{id}/project with the chosen project id on change', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    const select = screen.getByLabelText(/project/i)
    fireEvent.change(select, { target: { value: 'proj-b' } })
    await waitFor(() => expect(putCalls.length).toBe(1))
    expect(putCalls[0].url).toContain('/console/sandboxes/s1/project')
    expect(putCalls[0].body).toEqual({ project_id: 'proj-b' })
  })

  it('calls PUT with empty string when Unassigned is selected', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    const select = screen.getByLabelText(/project/i)
    fireEvent.change(select, { target: { value: '' } })
    await waitFor(() => expect(putCalls.length).toBe(1))
    expect(putCalls[0].body).toEqual({ project_id: '' })
  })
})
