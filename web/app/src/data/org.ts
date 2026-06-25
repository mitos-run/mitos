// Org-scoped data: members (role management) and projects.
// useSetRole is optimistic: it updates the cache before the server responds and
// rolls back on error so the UI feels instant but stays honest.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type MemberView, type ProjectView, type Role } from '../api'

export function useMembers() {
  return useQuery<MemberView[]>({ queryKey: ['members'], queryFn: () => api.members() })
}

export function useSetRole() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { accountId: string; role: Role }) => api.setMemberRole(v.accountId, v.role),
    onMutate: async (v) => {
      await qc.cancelQueries({ queryKey: ['members'] })
      const prev = qc.getQueryData<MemberView[]>(['members'])
      qc.setQueryData<MemberView[]>(['members'], (cur) =>
        (cur ?? []).map((m) => (m.account_id === v.accountId ? { ...m, role: v.role } : m)),
      )
      return { prev }
    },
    onError: (_e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(['members'], ctx.prev)
    },
    onSettled: () => void qc.invalidateQueries({ queryKey: ['members'] }),
  })
}

export function useProjects() {
  return useQuery<ProjectView[]>({ queryKey: ['projects'], queryFn: () => api.projects() })
}

export function useCreateProject() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { name: string; description: string }) => api.createProject(v.name, v.description),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['projects'] }),
  })
}
