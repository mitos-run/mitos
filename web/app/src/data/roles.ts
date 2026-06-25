// Data hooks for the roles permission-matrix view.
// useRoles fetches the builtin + custom role list from the BFF.
// useUpsertRole and useDeleteRole mutate custom roles and invalidate the cache.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type CustomRole, type RolesView } from '../api'

export function useRoles() {
  return useQuery<RolesView>({ queryKey: ['roles'], queryFn: () => api.roles() })
}

export function useUpsertRole() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (role: CustomRole) => api.upsertRole(role),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['roles'] }),
  })
}

export function useDeleteRole() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.deleteRole(name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['roles'] }),
  })
}
