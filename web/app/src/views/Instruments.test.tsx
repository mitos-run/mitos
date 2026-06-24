import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Instruments } from './Instruments'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(
    new Response(JSON.stringify({ org_id: 'o1', activate_p50_ms: 27, activate_p99_ms: 41, forks_served: 10, cow_savings_bytes: 2415919104, marginal_bytes_per_fork: 3145728 }), {
      status: 200, headers: { 'content-type': 'application/json' },
    }),
  )
})

describe('Instruments cockpit', () => {
  it('renders the measured activate latency and CoW density tiles', async () => {
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText('27')).toBeInTheDocument())
    expect(screen.getByText(/Activate P50/i)).toBeInTheDocument()
    expect(screen.getByText(/Activate P99/i)).toBeInTheDocument()
    expect(screen.getByText(/Forks served/i)).toBeInTheDocument()
    expect(screen.getByText('10')).toBeInTheDocument()
  })
})
