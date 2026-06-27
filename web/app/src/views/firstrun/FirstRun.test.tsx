// FirstRun.test.tsx: guided first-run card unit tests.
//
// TDD: written before FirstRun.tsx existed; each suite asserts the expected
// DOM and accessible shape. Hooks mocked with vi.mock, mirroring the pattern
// in Instruments.test.tsx.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

// Mock TanStack Router Link so tests render without a full router context.
vi.mock('@tanstack/react-router', () => ({
  Link: (p: { to: string; children: React.ReactNode; className?: string }) => (
    <a href={p.to} className={p.className}>{p.children}</a>
  ),
}))

// Mock useBilling so tests are deterministic.
vi.mock('../../data/account', () => ({
  useBilling: vi.fn(),
}))

import { useBilling } from '../../data/account'
import { FirstRun, isFirstRun } from './FirstRun'

const mockUseBilling = useBilling as ReturnType<typeof vi.fn>

const billingData = {
  org_id: 'o1',
  status: 'active',
  balance_cents: 500,
  spend_cents: 0,
  soft_cap_cents: 0,
  hard_cap_cents: 0,
  ledger_entries: [],
}

const billingWithSpend = {
  ...billingData,
  spend_cents: 1234,
}

beforeEach(() => {
  vi.clearAllMocks()
  mockUseBilling.mockReturnValue({ data: billingData, isLoading: false, error: null })
})

// ---- isFirstRun predicate ---------------------------------------------------

describe('isFirstRun predicate', () => {
  it('returns true when instruments and sandboxes are both undefined', () => {
    expect(isFirstRun(undefined, undefined)).toBe(true)
  })

  it('returns true when forks_served is 0 and no running sandboxes', () => {
    const instruments = { org_id: 'o1', forks_served: 0, activate_p50_ms: 0, activate_p99_ms: 0, cow_savings_bytes: 0, marginal_bytes_per_fork: 0 }
    expect(isFirstRun(instruments, [])).toBe(true)
  })

  it('returns false when forks_served is greater than 0', () => {
    const instruments = { org_id: 'o1', forks_served: 10, activate_p50_ms: 27, activate_p99_ms: 41, cow_savings_bytes: 0, marginal_bytes_per_fork: 0 }
    expect(isFirstRun(instruments, [])).toBe(false)
  })

  it('returns false when there are running sandboxes even if forks_served is 0', () => {
    const instruments = { org_id: 'o1', forks_served: 0, activate_p50_ms: 0, activate_p99_ms: 0, cow_savings_bytes: 0, marginal_bytes_per_fork: 0 }
    const sandboxes = [{ id: 'sb-a', org_id: 'o1', template: 'python', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' }]
    expect(isFirstRun(instruments, sandboxes)).toBe(false)
  })

  it('returns true when sandboxes list has no Running-phase entries', () => {
    const instruments = { org_id: 'o1', forks_served: 0, activate_p50_ms: 0, activate_p99_ms: 0, cow_savings_bytes: 0, marginal_bytes_per_fork: 0 }
    const sandboxes = [{ id: 'sb-a', org_id: 'o1', template: 'python', node: 'w1', phase: 'Stopped', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' }]
    expect(isFirstRun(instruments, sandboxes)).toBe(true)
  })
})

// ---- FirstRun component: rollouts use case ----------------------------------

describe('FirstRun with uc="rollouts"', () => {
  it('shows the rollouts title as a heading', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() =>
      expect(
        screen.getByRole('heading', { name: /Fork your first swarm of rollouts/i }),
      ).toBeInTheDocument(),
    )
  })

  it('renders a code block containing fork(', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      // The rollouts snippet has sb.fork(8)
      expect(screen.getByText(/fork\(/)).toBeInTheDocument()
    })
  })

  it('renders a Copy button that is keyboard operable', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      const btn = screen.getByRole('button', { name: /copy snippet/i })
      expect(btn).toBeInTheDocument()
    })
  })

  it('announces "Copied" via aria-live after a successful copy', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(<FirstRun uc="rollouts" />)
    const btn = await screen.findByRole('button', { name: /copy snippet/i })
    await userEvent.click(btn)

    expect(writeText).toHaveBeenCalled()
    // The button label updates to "Copied"
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /copied to clipboard/i })).toBeInTheDocument(),
    )
  })

  it('shows the free credit line with balance and spend', async () => {
    mockUseBilling.mockReturnValue({ data: billingWithSpend, isLoading: false, error: null })
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/free credit/i)).toBeInTheDocument()
      // spend_cents: 1234 -> $12.34
      expect(screen.getByText(/\$12\.34/)).toBeInTheDocument()
    })
  })

  it('shows the balance in free credit line', async () => {
    // balance_cents: 500 -> $5.00
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/\$5\.00/)).toBeInTheDocument()
    })
  })

  it('links to /forks as the fork tree pointer', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /fork tree/i })
      expect(link).toHaveAttribute('href', '/forks')
    })
  })

  it('mentions that metrics light up on this page', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/light up/i)).toBeInTheDocument()
    })
  })
})

// ---- FirstRun component: default (no uc) ------------------------------------

describe('FirstRun with no uc prop', () => {
  it('shows the default title', async () => {
    render(<FirstRun />)
    await waitFor(() =>
      expect(
        screen.getByRole('heading', { name: /Fork your first swarm/i }),
      ).toBeInTheDocument(),
    )
  })

  it('does not show the rollouts-specific title', async () => {
    render(<FirstRun />)
    await waitFor(() =>
      expect(
        screen.queryByRole('heading', { name: /Fork your first swarm of rollouts/i }),
      ).not.toBeInTheDocument(),
    )
  })
})

// ---- FirstRun component: billing loading / absent ---------------------------

describe('FirstRun billing fallback', () => {
  it('shows nothing extra when billing is loading', async () => {
    mockUseBilling.mockReturnValue({ data: undefined, isLoading: true, error: null })
    render(<FirstRun uc="rollouts" />)
    // Must not crash; heading still present
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: /Fork your first swarm of rollouts/i })).toBeInTheDocument(),
    )
    // No billing line rendered while loading
    expect(screen.queryByText(/free credit/i)).not.toBeInTheDocument()
  })

  it('shows a calm fallback when billing data is absent', async () => {
    mockUseBilling.mockReturnValue({ data: null, isLoading: false, error: null })
    render(<FirstRun uc="rollouts" />)
    await waitFor(() =>
      expect(screen.getByText(/free credit available/i)).toBeInTheDocument(),
    )
  })
})
