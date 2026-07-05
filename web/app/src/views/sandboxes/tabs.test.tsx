// Behavior tests for tabs.tsx: OverviewTab's sizing rows (the Sandbox CRD has
// no per-sandbox resource override today, Create only records the requested
// vcpu/mem as informational annotations, see
// internal/saas/console/clustersandbox/clustersandbox.go viewOf, so the
// overview must label these rows as requests, not as provisioned facts) and
// LogsTab's honest handling of a stream transport that hard-fails with 501.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { OverviewTab, LogsTab } from './tabs'
import type { SandboxView } from '../../api'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const sb: SandboxView = {
  id: 'sb-1',
  org_id: 'o1',
  template: 'python-3.12',
  node: 'w1',
  phase: 'Running',
  vcpus: 2,
  mem_bytes: 2 * 1024 ** 3,
  created_at: '2026-01-01T00:00:00Z',
}

describe('OverviewTab', () => {
  it('labels the vcpu and memory rows as requested, not provisioned', () => {
    render(<OverviewTab sb={sb} />)
    expect(screen.getByText('Requested vCPUs')).toBeInTheDocument()
    expect(screen.getByText('Requested memory')).toBeInTheDocument()
    expect(screen.queryByText(/^vcpus$/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/^memory$/i)).not.toBeInTheDocument()
  })

  // issue #712 phase 0: region is shown only when present, so a single-value
  // deployment (no label stamped at all) never shows an empty/misleading row.
  it('shows no Region row when the sandbox has none', () => {
    render(<OverviewTab sb={sb} />)
    expect(screen.queryByText('Region')).not.toBeInTheDocument()
  })

  it('shows the Region row when the sandbox carries one', () => {
    render(<OverviewTab sb={{ ...sb, region: 'fra' }} />)
    expect(screen.getByText('Region')).toBeInTheDocument()
    expect(screen.getByText('fra')).toBeInTheDocument()
  })
})

describe('LogsTab', () => {
  beforeEach(() => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.includes('/logs/stream')) return Promise.resolve(new Response(null, { status: 501 }))
      if (url.includes('/logs')) return Promise.resolve(new Response('boot ok', { status: 200 }))
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders an honest unsupported state (and still shows the snapshot) when the live stream 501s, with no reconnect spinner', async () => {
    wrap(<LogsTab id="sb-1" />)
    // Snapshot loads first, in the default (non-live) view.
    await waitFor(() => expect(screen.getByText('boot ok')).toBeInTheDocument())

    fireEvent.click(screen.getByLabelText(/live logs/i))

    await waitFor(() =>
      expect(
        screen.getByText(/live log streaming is not available on this deployment yet\. the snapshot below still works\./i),
      ).toBeInTheDocument(),
    )
    // Never claims to be reconnecting once the hard failure is known.
    expect(screen.queryByText(/reconnecting/i)).not.toBeInTheDocument()
    // The snapshot content is still visible underneath the notice.
    expect(screen.getByText('boot ok')).toBeInTheDocument()
  })
})
