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

// Mock useFirstActivity so tests are deterministic.
vi.mock('../../data/firstActivity', () => ({
  useFirstActivity: vi.fn(),
}))

import { useBilling } from '../../data/account'
import { useFirstActivity } from '../../data/firstActivity'
import { FirstRun, isFirstRun } from './FirstRun'

const mockUseBilling = useBilling as ReturnType<typeof vi.fn>
const mockUseFirstActivity = useFirstActivity as ReturnType<typeof vi.fn>

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

// jsdom does not implement window.matchMedia; stub it so Celebrate can read
// prefers-reduced-motion without throwing.
function mockMatchMedia(matches = false) {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    configurable: true,
    value: vi.fn().mockReturnValue({ matches }),
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  mockUseBilling.mockReturnValue({ data: billingData, isLoading: false, error: null })
  mockUseFirstActivity.mockReturnValue({ data: { active: false }, isLoading: false })
  mockMatchMedia(false)
  sessionStorage.clear()
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

  it('shows failure feedback when clipboard write rejects', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('clipboard denied'))
    Object.assign(navigator, { clipboard: { writeText } })

    render(<FirstRun uc="rollouts" />)
    const btn = await screen.findByRole('button', { name: /copy snippet/i })
    await userEvent.click(btn)

    expect(writeText).toHaveBeenCalled()
    await waitFor(() =>
      expect(screen.getByText(/Clipboard unavailable/i)).toBeInTheDocument(),
    )
  })

  it('shows failure feedback when clipboard is unavailable', async () => {
    // Simulate environments where navigator.clipboard is undefined.
    const originalClipboard = navigator.clipboard
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })

    render(<FirstRun uc="rollouts" />)
    const btn = await screen.findByRole('button', { name: /copy snippet/i })
    await userEvent.click(btn)

    await waitFor(() =>
      expect(screen.getByText(/Clipboard unavailable/i)).toBeInTheDocument(),
    )

    // Restore clipboard for subsequent tests.
    Object.defineProperty(navigator, 'clipboard', { value: originalClipboard, configurable: true })
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

  it('links to /forks when first call lands', async () => {
    mockUseFirstActivity.mockReturnValue({ data: { active: true }, isLoading: false })
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /open the fork tree/i })
      expect(link).toHaveAttribute('href', '/forks')
    })
  })

  it('does not show the fork-tree link while waiting for first call', async () => {
    // active: false is the beforeEach default
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: /open the fork tree/i })).not.toBeInTheDocument()
    })
  })

  it('shows waiting-for-first-call text in step 3', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/Waiting for your first call/i)).toBeInTheDocument()
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

// ---- Step 1: masked key copy ------------------------------------------------

describe('FirstRun step 1: masked key copy', () => {
  const TEST_KEY = 'mk_live_a1b2c3d4e5f6'

  beforeEach(() => {
    sessionStorage.setItem('mitos.firstKey', TEST_KEY)
  })

  it('shows the masked key prefix and bullets, not the raw tail', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      // Prefix (first 12 chars)
      expect(screen.getByText(/mk_live_a1b2/)).toBeInTheDocument()
    })
    // Raw tail must NOT be in the DOM
    expect(screen.queryByText(/c3d4e5f6/)).not.toBeInTheDocument()
  })

  it('does not render the raw key tail anywhere in the DOM', async () => {
    const { container } = render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/mk_live_a1b2/)).toBeInTheDocument()
    })
    expect(container.innerHTML).not.toContain('c3d4e5f6')
  })

  it('copies the full raw export line and marks step 1 done', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(<FirstRun uc="rollouts" />)

    const copyBtn = await screen.findByRole('button', { name: /copy.*key/i })
    await userEvent.click(copyBtn)

    expect(writeText).toHaveBeenCalledWith(
      expect.stringContaining('export MITOS_API_KEY=mk_live_a1b2c3d4e5f6'),
    )

    await waitFor(() => {
      const step = document.querySelector('[data-step="key"]')
      expect(step).toHaveAttribute('data-done', 'true')
    })
  })

  it('announces key copy via aria-live', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(<FirstRun uc="rollouts" />)
    const copyBtn = await screen.findByRole('button', { name: /copy.*key/i })
    await userEvent.click(copyBtn)

    await waitFor(() => {
      expect(screen.getByText(/API key copied/i)).toBeInTheDocument()
    })
  })
})

// ---- Step 1: no key fallback ------------------------------------------------

describe('FirstRun step 1: no key stashed', () => {
  it('shows a primary Create an API key to continue link to /keys', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /create an api key to continue/i })
      expect(link).toHaveAttribute('href', '/keys')
      // The action is the primary affordance: button-styled, not an inline link.
      expect(link).toHaveClass('firstrun-create-key-btn')
    })
  })

  it('explains why there is no key on record', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/shown only once, when it is created/i)).toBeInTheDocument()
    })
  })

  it('does not show a masked key line', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByRole('link', { name: /create an api key/i })).toBeInTheDocument()
    })
    // No export MITOS_API_KEY= visible
    expect(screen.queryByText(/MITOS_API_KEY=/)).not.toBeInTheDocument()
  })
})

// ---- Step 2: tabbed runtimes ------------------------------------------------

describe('FirstRun step 2: tabbed runtimes', () => {
  it('renders three runtime tabs', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /python/i })).toBeInTheDocument()
      expect(screen.getByRole('tab', { name: /typescript/i })).toBeInTheDocument()
      expect(screen.getByRole('tab', { name: /cli/i })).toBeInTheDocument()
    })
  })

  it('defaults to Python tab selected', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      const pythonTab = screen.getByRole('tab', { name: /python/i })
      expect(pythonTab).toHaveAttribute('aria-selected', 'true')
    })
  })

  it('shows the TypeScript snippet after clicking the TypeScript tab', async () => {
    render(<FirstRun uc="rollouts" />)
    const tsTab = await screen.findByRole('tab', { name: /typescript/i })
    await userEvent.click(tsTab)
    await waitFor(() => {
      // The TS rollouts snippet uses Promise.all and swarm.map
      expect(screen.getByText(/Promise\.all/)).toBeInTheDocument()
    })
  })

  it('copy snippet marks step 2 done', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    render(<FirstRun uc="rollouts" />)
    const copyBtn = await screen.findByRole('button', { name: /copy snippet/i })
    await userEvent.click(copyBtn)

    await waitFor(() => {
      const step = document.querySelector('[data-step="snippet"]')
      expect(step).toHaveAttribute('data-done', 'true')
    })
  })
})

// ---- Step 3: first call state -----------------------------------------------

describe('FirstRun step 3: first call', () => {
  it('shows waiting text when first call has not landed', async () => {
    // active: false is the beforeEach default
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/Waiting for your first call/i)).toBeInTheDocument()
    })
  })

  it('does not show the celebration status while waiting', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.queryByRole('status')).not.toBeInTheDocument()
    })
  })

  it('shows You are live status when first call lands', async () => {
    mockUseFirstActivity.mockReturnValue({ data: { active: true }, isLoading: false })
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('You are live')
    })
  })

  it('reveals next-step links when first call lands', async () => {
    mockUseFirstActivity.mockReturnValue({ data: { active: true }, isLoading: false })
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByRole('link', { name: /open the fork tree/i })).toHaveAttribute('href', '/forks')
      expect(screen.getByRole('link', { name: /view usage/i })).toHaveAttribute('href', '/usage')
      expect(screen.getByRole('link', { name: /add credits/i })).toHaveAttribute('href', '/billing')
    })
  })

  it('does not show next-step links while waiting', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() => {
      expect(screen.getByText(/Waiting for your first call/i)).toBeInTheDocument()
    })
    expect(screen.queryByRole('link', { name: /view usage/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /add credits/i })).not.toBeInTheDocument()
  })
})

// ---- No em or en dash in rendered text --------------------------------------

describe('FirstRun: copy and voice', () => {
  it('renders no em or en dashes in any text', async () => {
    render(<FirstRun uc="rollouts" />)
    await waitFor(() =>
      expect(
        screen.getByRole('heading', { name: /Fork your first swarm of rollouts/i }),
      ).toBeInTheDocument(),
    )
    const bodyText = document.body.textContent ?? ''
    expect(bodyText).not.toMatch(/—|–/)
  })
})
