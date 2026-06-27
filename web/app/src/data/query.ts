// The console data layer: one QueryClient and the capabilities hook every
// gating decision reads from. Stale-while-revalidate by default so navigation
// feels instant; the server remains the source of truth for capabilities.
import { QueryClient, useQuery } from '@tanstack/react-query'
import { api, type Capabilities, UnauthorizedError } from '../api'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      refetchOnWindowFocus: false,
      retry: 1,
    },
  },
})

export function useCapabilities() {
  return useQuery<Capabilities>({
    queryKey: ['capabilities'],
    queryFn: () => api.capabilities(),
    staleTime: Infinity, // capabilities change only on redeploy
    retry: (_count, err) => !(err instanceof UnauthorizedError),
  })
}
