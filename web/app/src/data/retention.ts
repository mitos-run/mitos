// Data hooks for the per-org data and retention policy.
// GC enforcement lives in the controller (issue #163); this only stores/exposes the policy.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type DataRetentionPolicy } from '../api'

export function useDataRetention() {
  return useQuery<DataRetentionPolicy>({ queryKey: ['data-retention'], queryFn: () => api.dataRetention() })
}

export function useSetDataRetention() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (policy: DataRetentionPolicy) => api.setDataRetention(policy),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['data-retention'] }),
  })
}
