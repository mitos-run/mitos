// The Sandboxes data layer: list, single, logs, and an optimistic terminate.
// Terminate removes the row from the list cache immediately and rolls back if the
// BFF rejects, so the UI feels instant but stays honest.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type SandboxView } from '../api'

export function useSandboxes() {
  return useQuery<SandboxView[]>({ queryKey: ['sandboxes'], queryFn: () => api.sandboxes(), refetchInterval: 10_000 })
}

export function useSandbox(id: string) {
  return useQuery<SandboxView>({ queryKey: ['sandbox', id], queryFn: () => api.sandbox(id), enabled: !!id })
}

export function useSandboxLogs(id: string) {
  return useQuery<string>({ queryKey: ['sandbox-logs', id], queryFn: () => api.sandboxLogs(id), enabled: !!id })
}

export function useTerminateSandbox() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.terminateSandbox(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['sandboxes'] })
      const prev = qc.getQueryData<SandboxView[]>(['sandboxes'])
      qc.setQueryData<SandboxView[]>(['sandboxes'], (cur) => (cur ?? []).filter((s) => s.id !== id))
      return { prev }
    },
    onError: (_e, _id, ctx) => {
      if (ctx?.prev) qc.setQueryData(['sandboxes'], ctx.prev)
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['sandboxes'] })
    },
  })
}
