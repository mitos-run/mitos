import { describe, it, expect } from 'vitest'
import { render, screen, act, waitForElementToBeRemoved } from '@testing-library/react'
import { ToastProvider, useToast } from './Toast'

function Trigger() {
  const { notify } = useToast()
  return <button onClick={() => notify('saved', 'ok')}>go</button>
}

describe('Toast', () => {
  it('shows a toast and auto-dismisses it', async () => {
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    )
    act(() => {
      screen.getByRole('button', { name: 'go' }).click()
    })
    expect(await screen.findByText('saved')).toBeInTheDocument()
    await waitForElementToBeRemoved(() => screen.queryByText('saved'), { timeout: 5000 })
  })
})
