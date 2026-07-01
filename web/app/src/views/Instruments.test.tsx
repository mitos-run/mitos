import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Instruments } from './Instruments'
import { fmtBytes } from '../api'

// Mock TanStack Router Link so Instruments renders without a full router context.
vi.mock('@tanstack/react-router', () => ({
  Link: (p: { to: string; children: React.ReactNode; className?: string }) =>
    <a href={p.to} className={p.className}>{p.children}</a>,
}))

// Mock the data hooks so tests are deterministic; the real hooks call fetch and
// go through react-query, which is fine for integration-style tests but makes
// it hard to test capability-gated panels in isolation.
vi.mock('../data/instruments', () => ({
  useInstruments: vi.fn(),
}))
vi.mock('../data/sandboxes', () => ({
  useSandboxes: vi.fn(),
}))
vi.mock('../data/account', () => ({
  useBilling: vi.fn(),
  useAudit: vi.fn(),
}))
vi.mock('../data/query', () => ({
  useCapabilities: vi.fn(),
}))

// Mock useFirstActivity so that when FirstRun mounts (new-org branch) it does
// not attempt a real network call.
vi.mock('../data/firstActivity', () => ({
  useFirstActivity: vi.fn(),
}))

import { useInstruments } from '../data/instruments'
import { useSandboxes } from '../data/sandboxes'
import { useBilling, useAudit } from '../data/account'
import { useCapabilities } from '../data/query'
import { useFirstActivity } from '../data/firstActivity'

const mockUseInstruments = useInstruments as ReturnType<typeof vi.fn>
const mockUseSandboxes = useSandboxes as ReturnType<typeof vi.fn>
const mockUseBilling = useBilling as ReturnType<typeof vi.fn>
const mockUseAudit = useAudit as ReturnType<typeof vi.fn>
const mockUseCapabilities = useCapabilities as ReturnType<typeof vi.fn>
const mockUseFirstActivity = useFirstActivity as ReturnType<typeof vi.fn>

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const instrumentsData = {
  org_id: 'o1',
  activate_p50_ms: 27,
  activate_p99_ms: 41,
  forks_served: 10,
  cow_savings_bytes: 2415919104,
  marginal_bytes_per_fork: 3145728,
}

const sandboxesData = [
  { id: 'sb-aaa', org_id: 'o1', template: 'python', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
  { id: 'sb-bbb', org_id: 'o1', template: 'python', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
  { id: 'sb-ccc', org_id: 'o1', template: 'python', node: 'w1', phase: 'Stopped', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
]

const auditData = [
  { org_id: 'o1', actor_id: 'alice', action: 'fork', target: 'sb-aaa', detail: '', at: '2026-01-01T10:00:00Z' },
  { org_id: 'o1', actor_id: 'bob', action: 'terminate', target: 'sb-bbb', detail: '', at: '2026-01-01T09:00:00Z' },
]

const billingData = {
  org_id: 'o1',
  status: 'active',
  balance_cents: 5000,
  spend_cents: 1234,
  soft_cap_cents: 0,
  hard_cap_cents: 0,
  ledger_entries: [],
}

const capsNoBilling = {
  edition: 'community' as const,
  billing: false,
  signup: false,
  teams: true,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: true,
  ownership: 'self-hosted' as const,
}

const capsWithBilling = { ...capsNoBilling, billing: true }

beforeEach(() => {
  vi.clearAllMocks()
  mockUseInstruments.mockReturnValue({ data: instrumentsData, isLoading: false, error: null })
  mockUseSandboxes.mockReturnValue({ data: sandboxesData, isLoading: false, error: null })
  mockUseBilling.mockReturnValue({ data: billingData, isLoading: false, error: null })
  mockUseAudit.mockReturnValue({ data: auditData, isLoading: false, error: null })
  mockUseCapabilities.mockReturnValue({ data: capsNoBilling, isLoading: false, error: null })
  // Stable default for useFirstActivity so FirstRun does not error when it mounts.
  mockUseFirstActivity.mockReturnValue({ data: { active: false }, isLoading: false })
})

describe('Instruments (Overview) cockpit', () => {
  it('renders the PageHeader with the Overview title', async () => {
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1, name: /Overview/i })).toBeInTheDocument())
  })

  it('renders the measured activate latency and CoW density tiles when data is present', async () => {
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText('27')).toBeInTheDocument())
    expect(screen.getByText(/Activate P50/i)).toBeInTheDocument()
    expect(screen.getByText(/Activate P99/i)).toBeInTheDocument()
    expect(screen.getByText('41')).toBeInTheDocument()
    expect(screen.getByText(/Forks served/i)).toBeInTheDocument()
    expect(screen.getByText('10')).toBeInTheDocument()
    expect(screen.getByText(fmtBytes(2415919104))).toBeInTheDocument()
    expect(screen.getByText(fmtBytes(3145728))).toBeInTheDocument()
  })

  it('shows an inline note instead of tiles when no measured signal exists, but still renders operational panels', async () => {
    mockUseInstruments.mockReturnValue({
      data: { ...instrumentsData, forks_served: 0, activate_p50_ms: 0 },
      isLoading: false,
      error: null,
    })
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/No measured signal yet/i)).toBeInTheDocument())
    // Operational panels still render
    expect(screen.getByText(/Running now/i)).toBeInTheDocument()
    expect(screen.getByText(/Recent activity/i)).toBeInTheDocument()
  })
})

describe('Running now panel', () => {
  it('counts only Running-phase sandboxes and shows the count prominently', async () => {
    wrap(<Instruments />)
    // 2 of 3 sandboxes are Running
    await waitFor(() => expect(screen.getByText('2')).toBeInTheDocument())
    expect(screen.getByText(/Running now/i)).toBeInTheDocument()
  })

  it('lists up to 5 running sandbox ids in monospace within the Running now panel', async () => {
    wrap(<Instruments />)
    // Wait for the panel to render; both Running sandboxes must appear (sb-aaa
    // also appears in the Recent activity panel as an audit target, so use
    // getAllByText and assert at least one match is a list item in the panel).
    await waitFor(() => {
      const matches = screen.getAllByText('sb-aaa')
      // At least one instance is the running-panel list item (li.mono)
      expect(matches.some((el) => el.tagName === 'LI')).toBe(true)
    })
    const bbbMatches = screen.getAllByText('sb-bbb')
    expect(bbbMatches.some((el) => el.tagName === 'LI')).toBe(true)
    // Stopped sandbox must NOT appear in the running list
    const cccLiItems = screen.queryAllByText('sb-ccc').filter((el) => el.tagName === 'LI')
    expect(cccLiItems).toHaveLength(0)
  })

  it('links to /sandboxes from the footer', async () => {
    wrap(<Instruments />)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /View sandboxes/i })
      expect(link).toBeInTheDocument()
      expect(link).toHaveAttribute('href', '/sandboxes')
    })
  })

  it('shows the empty state when no sandboxes are running', async () => {
    mockUseSandboxes.mockReturnValue({ data: [], isLoading: false, error: null })
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/No sandboxes running/i)).toBeInTheDocument())
  })
})

describe('Spend this month panel', () => {
  it('is hidden when capabilities.billing is false', async () => {
    mockUseCapabilities.mockReturnValue({ data: capsNoBilling, isLoading: false, error: null })
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/Running now/i)).toBeInTheDocument())
    expect(screen.queryByText(/Spend this month/i)).not.toBeInTheDocument()
  })

  it('is shown when capabilities.billing is true and displays formatted spend', async () => {
    mockUseCapabilities.mockReturnValue({ data: capsWithBilling, isLoading: false, error: null })
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/Spend this month/i)).toBeInTheDocument())
    // spend_cents: 1234 -> $12.34
    expect(screen.getByText('$12.34')).toBeInTheDocument()
    const link = screen.getByRole('link', { name: /View billing/i })
    expect(link).toHaveAttribute('href', '/billing')
  })
})

describe('Recent activity panel', () => {
  it('lists up to 5 recent audit events', async () => {
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/Recent activity/i)).toBeInTheDocument())
    // Each event shows actor_id action target
    expect(screen.getByText(/alice/i)).toBeInTheDocument()
    expect(screen.getByText(/bob/i)).toBeInTheDocument()
  })

  it('links to /audit from the footer', async () => {
    wrap(<Instruments />)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /View audit log/i })
      expect(link).toHaveAttribute('href', '/audit')
    })
  })

  it('shows the empty state when there are no audit events', async () => {
    mockUseAudit.mockReturnValue({ data: [], isLoading: false, error: null })
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/No activity yet/i)).toBeInTheDocument())
  })
})

// ---- FirstRun visibility on Overview -----------------------------------------

describe('FirstRun visibility on Overview', () => {
  it('hides the FirstRun card and shows normal panels for an active org (forks_served > 0)', async () => {
    // Default beforeEach: forks_served 10 + Running sandboxes -> isFirstRun false.
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText(/Running now/i)).toBeInTheDocument())
    expect(screen.getByText(/Recent activity/i)).toBeInTheDocument()
    // FirstRun heading must not be present.
    expect(screen.queryByRole('heading', { name: /Fork your first swarm/i })).not.toBeInTheDocument()
  })

  it('shows the FirstRun card for a new org with no forks and no sandboxes', async () => {
    // New-org state: forks_served 0, empty sandbox list.
    mockUseInstruments.mockReturnValue({
      data: { ...instrumentsData, forks_served: 0, activate_p50_ms: 0, activate_p99_ms: 0 },
      isLoading: false,
      error: null,
    })
    mockUseSandboxes.mockReturnValue({ data: [], isLoading: false, error: null })

    wrap(<Instruments />)

    // isFirstRun -> true -> <FirstRun /> mounts; useFirstActivity is mocked in beforeEach.
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: /Fork your first swarm/i })).toBeInTheDocument(),
    )
    // Normal operational panels still render alongside the FirstRun card.
    expect(screen.getByText(/Running now/i)).toBeInTheDocument()
  })
})
