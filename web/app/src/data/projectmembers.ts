// Project-scoped member hooks: list, assign, and revoke per-project membership.
// Mutations invalidate ['project-members', projectId] on success so the table
// refreshes without a manual reload.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ProjectMembership, type Role } from '../api'

export function useProjectMembers(projectId: string) {
  return useQuery<ProjectMembership[]>({
    queryKey: ['project-members', projectId],
    queryFn: () => api.projectMembers(projectId),
  })
}

export function useAssignProjectMember(projectId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { accountId: string; role: Role }) => api.assignProjectMember(projectId, v.accountId, v.role),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['project-members', projectId] }),
  })
}

export function useRevokeProjectMember(projectId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (accountId: string) => api.revokeProjectMember(projectId, accountId),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['project-members', projectId] }),
  })
}
