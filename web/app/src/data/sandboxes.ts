// The Sandboxes data layer: list, single, logs, and an optimistic terminate.
// Terminate removes the row from the list cache immediately and rolls back if the
// BFF rejects, so the UI feels instant but stays honest.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type CreateSandboxRequest, type SandboxView } from '../api'

export function useSandboxes() {
  return useQuery<SandboxView[]>({ queryKey: ['sandboxes'], queryFn: () => api.sandboxes(), refetchInterval: 10_000 })
}

export function useSandbox(id: string) {
  return useQuery<SandboxView>({ queryKey: ['sandbox', id], queryFn: () => api.sandbox(id), enabled: !!id })
}

export function useSandboxLogs(id: string) {
  return useQuery<string>({ queryKey: ['sandbox-logs', id], queryFn: () => api.sandboxLogs(id), enabled: !!id })
}

export function useSetSandboxProject() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; projectId: string }) => api.setSandboxProject(v.id, v.projectId),
    onSettled: (_data, _err, v) => {
      void qc.invalidateQueries({ queryKey: ['sandboxes'] })
      void qc.invalidateQueries({ queryKey: ['sandbox', v.id] })
    },
  })
}

// useCreateSandbox posts a new sandbox and, on success, inserts it into the
// cached list immediately (an optimistic-feeling insert without guessing the
// server-assigned id: it invalidates right after so the next poll reconciles
// with the real record).
export function useCreateSandbox() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: CreateSandboxRequest) => api.createSandbox(req),
    onSuccess: (created) => {
      qc.setQueryData<SandboxView[]>(['sandboxes'], (cur) => [...(cur ?? []), created])
      void qc.invalidateQueries({ queryKey: ['sandboxes'] })
    },
  })
}

// useForkSandbox creates count copies of a sandbox. The list and fork-tree
// queries are invalidated on success so the new nodes show up without waiting
// for the next poll.
export function useForkSandbox() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: string; count: number }) => api.forkSandbox(v.id, v.count),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sandboxes'] })
      void qc.invalidateQueries({ queryKey: ['forktree'] })
    },
  })
}

// useExecSandbox runs one command in a sandbox (the RunCommand panel). It is
// not cached: every submit is a fresh call.
export function useExecSandbox() {
  return useMutation({
    mutationFn: (v: { id: string; cmd: string; timeoutS?: number }) => api.execSandbox(v.id, v.cmd, v.timeoutS),
  })
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
