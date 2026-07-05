// The instance-operator plane's data layer: overview/orgs/nodes rollups, the
// signup waitlist, and this plane's own audit log, all gated server-side on
// the caller's admin capability (see routes.tsx's `when: (c) => c.admin`).
// Approve invalidates the waitlist AND the audit query, so an approved entry
// drops off the waitlist and the new admin.waitlist.approve event shows up
// immediately.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type AdminOverview, type AdminOrgsResponse, type AdminNodesResponse, type AdminWaitlistEntryView, type AuditEvent } from '../api'

export function useAdminOverview() {
  return useQuery<AdminOverview>({ queryKey: ['admin', 'overview'], queryFn: () => api.adminOverview() })
}

export function useAdminOrgs() {
  return useQuery<AdminOrgsResponse>({ queryKey: ['admin', 'orgs'], queryFn: () => api.adminOrgs() })
}

export function useAdminNodes() {
  return useQuery<AdminNodesResponse>({ queryKey: ['admin', 'nodes'], queryFn: () => api.adminNodes() })
}

export function useAdminWaitlist() {
  return useQuery<AdminWaitlistEntryView[]>({ queryKey: ['admin', 'waitlist'], queryFn: () => api.adminWaitlist() })
}

export function useApproveWaitlistEntry() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.approveWaitlistEntry(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['admin', 'waitlist'] })
      void qc.invalidateQueries({ queryKey: ['admin', 'audit'] })
    },
  })
}

export function useAdminAudit() {
  return useQuery<AuditEvent[]>({ queryKey: ['admin', 'audit'], queryFn: () => api.adminAudit() })
}
