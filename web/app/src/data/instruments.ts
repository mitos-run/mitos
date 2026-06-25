// The instrument cockpit's data source: the org's own measured activate latency
// and CoW density from the #211/#33 pipeline. Polls at a slow cadence so the
// cockpit stays live without hammering the BFF.
import { useQuery } from '@tanstack/react-query'
import { api, type Instruments } from '../api'

export function useInstruments() {
  return useQuery<Instruments>({
    queryKey: ['instruments'],
    queryFn: () => api.instruments(),
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
}
