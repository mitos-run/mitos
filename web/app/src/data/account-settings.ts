// Account-settings hooks: profile fetch/update, session list, and session revocation.
// useRevokeSession is optimistic: it removes the row from the cache before the
// server responds and rolls back on error.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type AccountView, type AccountPatch, type SessionView } from '../api'

export function useAccount() {
  return useQuery<AccountView>({ queryKey: ['account'], queryFn: () => api.account() })
}

export function useUpdateAccount() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (patch: AccountPatch) => api.updateAccount(patch),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['account'] }),
  })
}

export function useSessions() {
  return useQuery<SessionView[]>({ queryKey: ['sessions'], queryFn: () => api.sessions() })
}

export function useRevokeSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.revokeSession(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['sessions'] })
      const prev = qc.getQueryData<SessionView[]>(['sessions'])
      qc.setQueryData<SessionView[]>(['sessions'], (cur) => (cur ?? []).filter((s) => s.id !== id))
      return { prev }
    },
    onError: (_e, _id, ctx) => {
      if (ctx?.prev) qc.setQueryData(['sessions'], ctx.prev)
    },
    onSettled: () => void qc.invalidateQueries({ queryKey: ['sessions'] }),
  })
}

export function useRevokeAllSessions() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.revokeAllSessions(),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['sessions'] }),
  })
}

export function useSignOut() {
  return useMutation({
    mutationFn: () => api.revokeAllSessions(),
    onSuccess: () => { window.location.assign('/') },
  })
}
