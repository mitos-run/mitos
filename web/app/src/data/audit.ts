// Audit-specific data hooks: retention, sinks. Event list is in account.ts.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type SinkView, type SinkType } from '../api'

export function useAuditRetention() {
  return useQuery<{ days: number }>({ queryKey: ['audit-retention'], queryFn: () => api.auditRetention() })
}

export function useSetRetention() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (days: number) => api.setAuditRetention(days),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['audit-retention'] }),
  })
}

export function useAuditSinks() {
  return useQuery<SinkView[]>({ queryKey: ['audit-sinks'], queryFn: () => api.auditSinks() })
}

export function useAddSink() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { type: SinkType; endpoint: string }) => api.addAuditSink(v.type, v.endpoint),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['audit-sinks'] }),
  })
}

export function useDeleteSink() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.deleteAuditSink(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['audit-sinks'] })
      const prev = qc.getQueryData<SinkView[]>(['audit-sinks'])
      qc.setQueryData<SinkView[]>(['audit-sinks'], (cur) => (cur ?? []).filter((s) => s.id !== id))
      return { prev }
    },
    onError: (_e, _id, ctx) => {
      if (ctx?.prev) qc.setQueryData(['audit-sinks'], ctx.prev)
    },
    onSettled: () => void qc.invalidateQueries({ queryKey: ['audit-sinks'] }),
  })
}
