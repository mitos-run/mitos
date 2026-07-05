// Thin typed client over the org-scoped BFF (cmd/console). The server enforces
// org isolation and capability gating; this client is a dumb fetch wrapper.

// Thrown when the server returns HTTP 401 (no session). Callers that need to
// distinguish unauthenticated from other errors can check instanceof.
export class UnauthorizedError extends Error {
  constructor(path: string) {
    super(`${path}: 401 Unauthorized`)
    this.name = 'UnauthorizedError'
  }
}

// Entitlements is the resolved set of plan-gated hosted conveniences for the
// caller's org (mitos.run/mitos/internal/saas/billing.Entitlements). On the
// self-hosted community edition every field is on with auditRetentionDays 0
// (unlimited): the engine is never gated by plan.
export type Entitlements = {
  ssoEnforced: boolean
  scim: boolean
  auditStreaming: boolean
  auditRetentionDays: number
  seatPriceCents: number
}

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
  // authConnectors is the sorted list of configured social-login providers.
  // Present when the server is new enough to return it; absent on old deploys.
  authConnectors?: string[]
  // plan and entitlements are present when the server is new enough to
  // return them; absent on old deploys. plan is "free" or "team" on a hosted
  // deployment (informational only on community, which is never plan-gated).
  plan?: 'free' | 'team'
  entitlements?: Entitlements
  // admin is true when the CALLER holds the instance-operator capability; it
  // gates the "Operate" nav group and /admin/* routes. Optional (absent on
  // older servers), like authConnectors/plan/entitlements above.
  admin?: boolean
  // feedback tells the FeedbackButton dialog where composed feedback goes:
  // a mailto: (channel "email", hosted default) or a GitHub new-issue link
  // (channel "github", community default). Optional (absent on older
  // servers); FeedbackButton hides itself until this is present.
  feedback?: FeedbackCapability
  // version is the console binary's build version ("dev" for an unreleased
  // build). Optional (absent on older servers); rendered in the sidebar
  // footer and included in feedback diagnostics.
  version?: string
}

export type FeedbackCapability = {
  channel: 'email' | 'github'
  target: string
}

export type AuthConnectorsResponse = {
  connectors: string[]
  // signup mirrors the server-controlled caps.signup flag over the public
  // pre-auth endpoint (the authenticated /console/capabilities is not readable
  // before login in production). Absent on older servers.
  signup?: boolean
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
  project_id?: string
}

// CreateSandboxRequest is the body of POST /console/sandboxes. vcpus/mem_gib
// must be one of the static options the server validates (1/2/4 vCPU;
// 1/2/4/8 GiB); project_id is optional (empty means unassigned).
export type CreateSandboxRequest = {
  template: string
  vcpus: number
  mem_gib: number
  project_id?: string
}

export type ForkResult = { org_id: string; source: string; ids: string[] }

export type ExecResult = { stdout: string; stderr: string; exit_code: number }

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
export type AuditEvent = {
  org_id: string
  actor_id: string
  actor_name?: string
  actor_type?: 'user' | 'api_key' | 'system'
  action: string
  target: string
  target_type?: string
  target_name?: string
  detail: string
  at: string
}
export type TemplateView = { name: string; org_id: string; description: string; image: string; updated_at: string }
// BoxView is one entry in the Box reservation catalog (illustrative pricing;
// see mitos.run/mitos/internal/saas/billing.Reservation).
export type BoxView = { key: string; vcpu: number; mem_gib: number; monthly_cents: number }
export type UsageResponse = { org_id: string; records: unknown[]; totals: Record<string, number>; cost: Record<string, number> }
export type BillingView = { org_id: string; status: string; balance_cents: number; spend_cents: number; soft_cap_cents: number; hard_cap_cents: number; ledger_entries: Array<{ ts?: string; cents?: number; reason?: string }>; topup_available: boolean }

export type Role = 'owner' | 'admin' | 'billing' | 'member' | 'viewer'
export type MemberView = { account_id: string; org_id: string; role: Role; created_at: string; email?: string; display_name?: string }

export type InvitationState = 'pending' | 'accepted' | 'expired' | 'revoked'
export type InvitationView = {
  id: string
  org_id: string
  email: string
  role: Role
  state: InvitationState
  inviter_id: string
  inviter_name: string
  created_at: string
  expires_at: string
}
// InviteLookupView is the PUBLIC, pre-auth summary returned by
// GET /console/invites/lookup. email_hint is partially masked (e.g.
// "jo***@example.com"); the full email is never returned before accept.
export type InviteLookupView = {
  org_name: string
  inviter_name: string
  email_hint: string
  role: Role
  state: InvitationState
}
export type ProjectView = { id: string; org_id: string; name: string; description: string; created_at: string }
export type ProjectMembership = { account_id: string; project_id: string; role: Role }

export type SinkType = 'webhook' | 's3' | 'splunk' | 'datadog'
export type SinkView = { id: string; org_id: string; type: SinkType; endpoint: string; enabled: boolean; created_at: string }

export type FirstActivity = { active: boolean }

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

// --- Instance-operator plane (/console/admin/...) ---

export type AdminOverview = {
  orgs: number
  running_sandboxes: number
  // null when no NodeSource is configured on this deployment (an honest
  // "not available" rather than a fabricated 0).
  nodes_ready: number | null
  nodes_total: number | null
  signup_mode: 'open' | 'waitlist'
}

export type AdminOrgView = {
  id: string
  name: string
  tier: string
  members: number
  running: number
  month_usage_cents: number
}

export type AdminNodeView = {
  name: string
  ready: boolean
  kvm: boolean
  dedicated: boolean
  allocatable_cpu: string
  allocatable_mem: string
}

export type AdminNodesResponse = {
  available: boolean
  nodes: AdminNodeView[]
}

export type AdminWaitlistEntryView = {
  id: string
  email: string
  created_at: string
}

async function get<T>(path: string): Promise<T> {
  const r = await fetch(path, { credentials: 'same-origin' })
  if (r.status === 401) throw new UnauthorizedError(path)
  if (!r.ok) throw new Error(`${path}: ${r.status}`)
  return (await r.json()) as T
}

// Generic POST helper mirroring get<T>. Returns the parsed JSON body, or null
// when the server replies with no content (e.g. 202 Accepted with empty body).
export async function post<T>(path: string, body: unknown): Promise<T | null> {
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (r.status === 401) throw new UnauthorizedError(path)
  if (!r.ok) throw new Error(`${path}: ${r.status}`)
  const text = await r.text()
  return text ? (JSON.parse(text) as T) : null
}

// apiErrorMessage extracts the console's apierr envelope's cause (the
// actionable, non-secret detail: "vcpus must be one of 1, 2, 4") from a
// failed response, falling back to a generic "<op>: <status>" message when the
// body is not the expected shape.
async function apiErrorMessage(r: Response, op: string): Promise<string> {
  const body = await r.json().catch(() => null)
  return body?.error?.cause ?? body?.error?.message ?? `${op}: ${r.status}`
}

export const api = {
  capabilities: () => get<Capabilities>('/console/capabilities'),
  // authConfig fetches the public /auth/connectors endpoint which returns the
  // configured social-login providers and the server-controlled signup flag
  // without requiring a session. The SPA reads this before login to decide
  // which social buttons to render and whether to offer self-serve signup.
  authConfig: () => get<AuthConnectorsResponse>('/auth/connectors'),
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
  setSandboxProject: async (id: string, projectId: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/project`, {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ project_id: projectId }),
    })
    if (!r.ok) throw new Error(`set sandbox project: ${r.status}`)
  },
  sandboxLogs: async (id: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/logs`, { credentials: 'same-origin' })
    if (!r.ok) throw new Error(`logs: ${r.status}`)
    return r.text()
  },
  // logStreamURL is the SSE endpoint useLogStream's EventSource connects to.
  // It is same-origin and cookie-authenticated, so no token needs to travel in
  // the URL.
  logStreamURL: (id: string) => `/console/sandboxes/${encodeURIComponent(id)}/logs/stream`,
  createSandbox: async (req: CreateSandboxRequest) => {
    const r = await fetch('/console/sandboxes', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(req),
    })
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'create sandbox'))
    return (await r.json()) as SandboxView
  },
  forkSandbox: async (id: string, count: number) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/fork`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ count }),
    })
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'fork sandbox'))
    return (await r.json()) as ForkResult
  },
  execSandbox: async (id: string, cmd: string, timeoutS?: number) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/exec`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cmd, timeout_s: timeoutS ?? 0 }),
    })
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'exec'))
    return (await r.json()) as ExecResult
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
  boxes: () => get<{ boxes: BoxView[] }>('/console/boxes').then((r) => r.boxes ?? []),
  billing: () => get<BillingView>('/console/billing'),
  billingPortal: () => get<{ url: string }>('/console/billing/portal').then((r) => r.url),
  topupUrl: (amountCents: number) =>
    get<{ url: string }>(`/console/billing/topup?amount=${amountCents}`).then((r) => r.url),
  setSpendCap: (softCents: number, hardCents: number) =>
    post<{ org_id: string }>(
      '/console/billing/spend-cap',
      { soft_cents: softCents, hard_cents: hardCents },
    ),
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
  removeMember: async (accountId: string) => {
    const r = await fetch(`/console/members/${encodeURIComponent(accountId)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`remove member: ${r.status}`)
  },
  invites: () => get<{ org_id: string; invitations: InvitationView[] }>('/console/invites').then((r) => r.invitations ?? []),
  createInvite: async (email: string, role: Role) => {
    const r = await fetch('/console/invites', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ email, role }),
    })
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'create invite'))
    return (await r.json()) as InvitationView
  },
  revokeInvite: async (id: string) => {
    const r = await fetch(`/console/invites/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      credentials: 'same-origin',
    })
    if (!r.ok && r.status !== 204) throw new Error(`revoke invite: ${r.status}`)
  },
  resendInvite: async (id: string) => {
    const r = await fetch(`/console/invites/${encodeURIComponent(id)}/resend`, {
      method: 'POST',
      credentials: 'same-origin',
    })
    if (!r.ok) throw new Error(`resend invite: ${r.status}`)
    return (await r.json()) as InvitationView
  },
  // inviteLookup is the PUBLIC pre-auth call: no credentials are required
  // (and none are sent), matching the server route mounted outside session
  // auth.
  inviteLookup: async (token: string) => {
    const r = await fetch(`/console/invites/lookup?token=${encodeURIComponent(token)}`)
    if (!r.ok) throw new Error(`invite lookup: ${r.status}`)
    return (await r.json()) as InviteLookupView
  },
  acceptInvite: (token: string) => post<{ org_id: string; role: Role }>('/console/invites/accept', { token }),
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
    // Surfaces the apierr cause (e.g. "audit-sink streaming requires the Team
    // plan" on a 402) instead of a bare status code, so a plan-gated org sees
    // an honest, actionable message rather than a generic failure.
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'add sink'))
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
  adminOverview: () => get<AdminOverview>('/console/admin/overview'),
  adminOrgs: () => get<{ orgs: AdminOrgView[]; total: number }>('/console/admin/orgs'),
  adminNodes: () => get<AdminNodesResponse>('/console/admin/nodes'),
  adminWaitlist: () => get<{ entries: AdminWaitlistEntryView[] }>('/console/admin/waitlist').then((r) => r.entries ?? []),
  approveWaitlistEntry: async (id: string) => {
    const r = await fetch(`/console/admin/waitlist/${encodeURIComponent(id)}/approve`, {
      method: 'POST',
      credentials: 'same-origin',
    })
    if (!r.ok) throw new Error(await apiErrorMessage(r, 'approve waitlist entry'))
    return (await r.json()) as { email: string; approved: boolean }
  },
}

export async function firstActivity(): Promise<FirstActivity> {
  return get<FirstActivity>('/console/first-activity')
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / Math.pow(1024, i)).toFixed(1)} ${u[i]}`
}

export function fmtDollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}
