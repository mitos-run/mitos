// Shared test helper: render a subtree inside a fresh QueryClient so component
// tests do not share cache state. Router-aware helpers are added in Task 4.
import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { createConsoleRouter } from '../router'
import { ToastProvider } from '../ui/Toast'
import type { Capabilities } from '../api'

// Render the app at a given path with a given capabilities document, inside the
// query and toast providers. Used by AppShell, CommandPalette, and view tests.
export async function renderAt(path: string, caps: Capabilities) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createConsoleRouter(caps)
  await router.navigate({ to: path })
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <RouterProvider router={router} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}
