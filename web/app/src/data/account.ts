// Account-scoped data: api keys (with the create-once flow), usage, audit,
// templates, billing, spend cap, and the Box catalog. Mutations invalidate
// their list; revoke is optimistic.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type KeyView, type AuditEvent, type TemplateView, type UsageResponse, type BillingView, type BoxView } from '../api'

export function useKeys() { return useQuery<KeyView[]>({ queryKey: ['keys'], queryFn: () => api.keys() }) }
export function useUsage() { return useQuery<UsageResponse>({ queryKey: ['usage'], queryFn: () => api.usage() }) }
export function useAudit() { return useQuery<AuditEvent[]>({ queryKey: ['audit'], queryFn: () => api.audit() }) }
export function useTemplates() { return useQuery<TemplateView[]>({ queryKey: ['templates'], queryFn: () => api.templates() }) }
export function useBilling() { return useQuery<BillingView>({ queryKey: ['billing'], queryFn: () => api.billing() }) }
export function useBoxes() { return useQuery<BoxView[]>({ queryKey: ['boxes'], queryFn: () => api.boxes() }) }

export function useSetSpendCap() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ softCents, hardCents }: { softCents: number; hardCents: number }) =>
      api.setSpendCap(softCents, hardCents),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['billing'] }),
  })
}

export function useCreateKey() {
  const qc = useQueryClient()
  return useMutation({ mutationFn: (v: { name: string; scopes: string[]; ttlSeconds: number }) => api.createKey(v.name, v.scopes, v.ttlSeconds), onSuccess: () => void qc.invalidateQueries({ queryKey: ['keys'] }) })
}

export function useRevokeKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.revokeKey(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['keys'] })
      const prev = qc.getQueryData<KeyView[]>(['keys'])
      qc.setQueryData<KeyView[]>(['keys'], (cur) => (cur ?? []).map((k) => (k.id === id ? { ...k, revoked: true } : k)))
      return { prev }
    },
    onError: (_e, _id, ctx) => { if (ctx?.prev) qc.setQueryData(['keys'], ctx.prev) },
    onSettled: () => void qc.invalidateQueries({ queryKey: ['keys'] }),
  })
}
