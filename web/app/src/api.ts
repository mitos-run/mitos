// Thin typed client over the org-scoped BFF (cmd/console). The server enforces
// org isolation and capability gating; this client is a dumb fetch wrapper.

export type Capabilities = {
  edition: 'community' | 'hosted'
  billing: boolean
  signup: boolean
  teams: boolean
  idp: string
  orgSwitcher: boolean
  secrets: { providers: string[] }
  proof: boolean
  ownership: 'self-hosted' | 'hosted'
}

export type Instruments = {
  org_id: string
  activate_p50_ms: number
  activate_p99_ms: number
  forks_served: number
  cow_savings_bytes: number
  marginal_bytes_per_fork: number
}

export type SecretView = {
  name: string
  org_id: string
  provider: string
  mode: string
  version: number
  fingerprint: string
}

export type SandboxView = {
  id: string
  org_id: string
  template: string
  node: string
  phase: string
  vcpus: number
  mem_bytes: number
  created_at: string
}

export type ForkNode = {
  id: string
  parent_id: string
  phase: string
  private_dirty_bytes: number
  shared_bytes: number
}

export type ForkTree = { org_id: string; nodes: ForkNode[] }

export type KeyView = { id: string; name: string; prefix: string; scopes: string[]; created_at: string; expires_at?: string; revoked_at?: string; revoked: boolean }
export type CreateKeyResult = { org_id: string; raw_key: string; key: KeyView }
export type AuditEvent = { org_id: string; actor_id: string; action: string; target: string; detail: string; at: string }
export type TemplateView = { name: string; org_id: string; description: string; image: string; updated_at: string }
export type UsageResponse = { org_id: string; records: unknown[]; totals: Record<string, number>; cost: Record<string, number> }
export type BillingView = { org_id: string; status: string; balance_cents: number; spend_cents: number; soft_cap_cents: number; hard_cap_cents: number; ledger_entries: Array<{ ts?: string; cents?: number; reason?: string }> }

export type Role = 'owner' | 'admin' | 'billing' | 'member' | 'viewer'
export type MemberView = { account_id: string; org_id: string; role: Role; created_at: string }
export type ProjectView = { id: string; org_id: string; name: string; description: string; created_at: string }
export type ProjectMembership = { account_id: string; project_id: string; role: Role }

export type SinkType = 'webhook' | 's3' | 'splunk' | 'datadog'
export type SinkView = { id: string; org_id: string; type: SinkType; endpoint: string; enabled: boolean; created_at: string }

export type DataRetentionPolicy = {
  sandbox_metadata_days: number
  logs_days: number
  usage_days: number
  legal_hold: boolean
}

export type Permission =
  | 'members.manage'
  | 'projects.manage'
  | 'secrets.manage'
  | 'settings.manage'
  | 'billing.manage'
  | 'resources.use'
  | 'read'

export type CustomRole = {
  name: string
  permissions: string[]
}

export type RolesView = {
  org_id: string
  builtins: CustomRole[]
  custom: CustomRole[]
}

export type AccountView = {
  account_id: string
  email: string
  display_name: string
  timezone: string
  locale: string
  memberships: MemberView[]
}

export type AccountPatch = {
  display_name?: string
  timezone?: string
  locale?: string
}

export type SessionView = {
  id: string
  label: string
  created_at: string
  current: boolean
}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { credentials: 'same-origin' })
  if (!r.ok) throw new Error(`${path}: ${r.status}`)
  return (await r.json()) as T
}

export const api = {
  capabilities: () => get<Capabilities>('/console/capabilities'),
  instruments: () => get<Instruments>('/console/instruments'),
  secrets: () => get<{ secrets: SecretView[] }>('/console/secrets').then((r) => r.secrets ?? []),
  createSecret: async (name: string, value: string) => {
    const r = await fetch('/console/secrets', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, value }),
    })
    if (!r.ok) throw new Error(`create secret: ${r.status}`)
    return (await r.json()) as SecretView
  },
  deleteSecret: async (name: string) => {
    const r = await fetch(`/console/secrets/${encodeURIComponent(name)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`delete secret: ${r.status}`)
  },
  sandboxes: () => get<{ sandboxes: SandboxView[] }>('/console/sandboxes').then((r) => r.sandboxes ?? []),
  sandbox: (id: string) => get<SandboxView>(`/console/sandboxes/${encodeURIComponent(id)}`),
  terminateSandbox: async (id: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' })
    if (!r.ok && r.status !== 204) throw new Error(`terminate: ${r.status}`)
  },
  sandboxLogs: async (id: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/logs`, { credentials: 'same-origin' })
    if (!r.ok) throw new Error(`logs: ${r.status}`)
    return r.text()
  },
  forktree: () => get<ForkTree>('/console/forktree'),
  keys: () => get<{ keys: KeyView[] }>('/console/keys').then((r) => r.keys ?? []),
  createKey: async (name: string, scopes: string[], ttlSeconds: number) => {
    const r = await fetch('/console/keys', { method: 'POST', credentials: 'same-origin', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name, scopes, ttl_seconds: ttlSeconds }) })
    if (!r.ok) throw new Error(`create key: ${r.status}`)
    return (await r.json()) as CreateKeyResult
  },
  revokeKey: async (id: string) => {
    const r = await fetch(`/console/keys/${encodeURIComponent(id)}/revoke`, { method: 'POST', credentials: 'same-origin' })
    if (!r.ok) throw new Error(`revoke: ${r.status}`)
  },
  usage: () => get<UsageResponse>('/console/usage?from=&to='),
  audit: () => get<{ events: AuditEvent[] }>('/console/audit').then((r) => r.events ?? []),
  templates: () => get<{ templates: TemplateView[] }>('/console/templates').then((r) => r.templates ?? []),
  billing: () => get<BillingView>('/console/billing'),
  billingPortal: () => get<{ url: string }>('/console/billing/portal').then((r) => r.url),
  members: () => get<{ org_id: string; members: MemberView[] }>('/console/members').then((r) => r.members ?? []),
  setMemberRole: async (accountId: string, role: Role) => {
    const r = await fetch(`/console/members/${encodeURIComponent(accountId)}/role`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ role }),
    })
    if (!r.ok) throw new Error(`set role: ${r.status}`)
  },
  projects: () => get<{ org_id: string; projects: ProjectView[] }>('/console/projects').then((r) => r.projects ?? []),
  createProject: async (name: string, description: string) => {
    const r = await fetch('/console/projects', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, description }),
    })
    if (!r.ok) throw new Error(`create project: ${r.status}`)
    return (await r.json()) as ProjectView
  },
  account: () => get<AccountView>('/console/account'),
  updateAccount: async (patch: AccountPatch) => {
    const r = await fetch('/console/account', {
      method: 'PATCH',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(patch),
    })
    if (!r.ok) throw new Error(`update account: ${r.status}`)
    return (await r.json()) as AccountView
  },
  sessions: () => get<{ sessions: SessionView[] }>('/console/account/sessions').then((r) => r.sessions ?? []),
  revokeSession: async (id: string) => {
    const r = await fetch(`/console/account/sessions/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`revoke session: ${r.status}`)
  },
  revokeAllSessions: async () => {
    const r = await fetch('/console/account/sessions', {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`revoke all sessions: ${r.status}`)
  },
  auditRetention: () => get<{ days: number }>('/console/audit/retention'),
  setAuditRetention: async (days: number) => {
    const r = await fetch('/console/audit/retention', {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ days }),
    })
    if (!r.ok) throw new Error(`set retention: ${r.status}`)
  },
  auditSinks: () => get<{ sinks: SinkView[] }>('/console/audit/sinks').then((r) => r.sinks ?? []),
  addAuditSink: async (type: SinkType, endpoint: string) => {
    const r = await fetch('/console/audit/sinks', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ type, endpoint }),
    })
    if (!r.ok) throw new Error(`add sink: ${r.status}`)
    return (await r.json()) as SinkView
  },
  deleteAuditSink: async (id: string) => {
    const r = await fetch(`/console/audit/sinks/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`delete sink: ${r.status}`)
  },
  roles: () => get<RolesView>('/console/roles'),
  upsertRole: async (role: CustomRole) => {
    const r = await fetch('/console/roles', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(role),
    })
    if (!r.ok) throw new Error(`upsert role: ${r.status}`)
  },
  deleteRole: async (name: string) => {
    const r = await fetch(`/console/roles/${encodeURIComponent(name)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`delete role: ${r.status}`)
  },
  auditExportUrl: () => '/console/audit/export',
  dataRetention: () => get<DataRetentionPolicy>('/console/retention'),
  setDataRetention: async (policy: DataRetentionPolicy) => {
    const r = await fetch('/console/retention', {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(policy),
    })
    if (!r.ok) throw new Error(`set retention policy: ${r.status}`)
    return (await r.json()) as DataRetentionPolicy
  },
  projectMembers: (projectId: string) =>
    get<{ project_id: string; members: ProjectMembership[] }>(`/console/projects/${encodeURIComponent(projectId)}/members`).then((r) => r.members ?? []),
  assignProjectMember: async (projectId: string, accountId: string, role: Role) => {
    const r = await fetch(`/console/projects/${encodeURIComponent(projectId)}/members`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ account_id: accountId, role }),
    })
    if (!r.ok) throw new Error(`assign project member: ${r.status}`)
  },
  revokeProjectMember: async (projectId: string, accountId: string) => {
    const r = await fetch(`/console/projects/${encodeURIComponent(projectId)}/members/${encodeURIComponent(accountId)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`revoke project member: ${r.status}`)
  },
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / Math.pow(1024, i)).toFixed(1)} ${u[i]}`
}
