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
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / Math.pow(1024, i)).toFixed(1)} ${u[i]}`
}
