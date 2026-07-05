// The single source of truth for the console's information architecture. Both
// the nav (AppShell) and the router (router.tsx) derive from this array, so a
// route is declared exactly once. `when` gates a route on the server-advertised
// capabilities document; a route with no `when` is always present.
import type { Capabilities } from '../api'
import { Instruments } from '../views/Instruments'
import { SandboxList } from '../views/sandboxes/SandboxList'
import { ForkTree } from '../views/forktree/ForkTree'
import { Secrets } from '../views/Secrets'
import { Placeholder } from '../views/Placeholder'
import { SandboxDetail } from '../views/sandboxes/SandboxDetail'
import { Keys } from '../views/Keys'
import { Usage } from '../views/Usage'
import { Audit } from '../views/Audit'
import { Templates } from '../views/Templates'
import { Billing } from '../views/Billing'
import { Members } from '../views/Members'
import { Projects } from '../views/Projects'
import { Settings } from '../views/Settings'
import { Retention } from '../views/Retention'
import { Roles } from '../views/Roles'
import { ProjectDetail } from '../views/projects/ProjectDetail'
import { AcceptInvite } from '../auth/AcceptInvite'
import { AdminOverview } from '../views/admin/Overview'
import { AdminOrgs } from '../views/admin/Orgs'
import { AdminNodes } from '../views/admin/Nodes'
import { AdminWaitlist } from '../views/admin/Waitlist'
import { AdminAudit } from '../views/admin/Audit'

export type NavGroupName = 'Run' | 'Build' | 'Govern' | 'Billing' | 'Operate'
export const GROUP_ORDER: NavGroupName[] = ['Run', 'Build', 'Govern', 'Billing', 'Operate']

export type RouteDef = {
  path: string
  label: string
  group: NavGroupName
  element: () => JSX.Element
  when?: (c: Capabilities) => boolean
  hidden?: boolean
}

export const ROUTES: RouteDef[] = [
  { path: '/', label: 'Overview', group: 'Run', element: () => <Instruments /> },
  { path: '/sandboxes', label: 'Sandboxes', group: 'Run', element: () => <SandboxList /> },
  { path: '/sandboxes/$id', label: 'Sandbox', group: 'Run', element: () => <SandboxDetail />, hidden: true },
  { path: '/forks', label: 'Fork tree', group: 'Run', element: () => <ForkTree /> },
  { path: '/workspaces', label: 'Workspaces', group: 'Build', element: () => (
    <Placeholder
      title="Workspaces"
      description="Versioned filesystems your sandboxes share and keep between runs: browse them, compare revisions, and revert."
      today={<>Until then, manage workspaces from the CLI: <code>mitos ws create &lt;name&gt;</code> makes one and <code>mitos ws ls</code> lists what you have.</>}
    />
  ) },
  { path: '/templates', label: 'Templates', group: 'Build', element: () => <Templates /> },
  { path: '/secrets', label: 'Secrets', group: 'Build', element: () => <Secrets /> },
  { path: '/keys', label: 'API keys', group: 'Build', element: () => <Keys /> },
  { path: '/members', label: 'Members', group: 'Govern', element: () => <Members />, when: (c) => c.teams },
  { path: '/projects', label: 'Projects', group: 'Govern', element: () => <Projects />, when: (c) => c.teams },
  { path: '/projects/$id', label: 'Project', group: 'Govern', element: () => <ProjectDetail />, hidden: true },
  { path: '/audit', label: 'Audit', group: 'Govern', element: () => <Audit /> },
  { path: '/retention', label: 'Data and retention', group: 'Govern', element: () => <Retention /> },
  { path: '/roles', label: 'Roles', group: 'Govern', element: () => <Roles />, when: (c) => c.teams },
  { path: '/usage', label: 'Usage', group: 'Billing', element: () => <Usage /> },
  { path: '/billing', label: 'Billing', group: 'Billing', element: () => <Billing />, when: (c) => c.billing },
  // Account settings is reached from the top-bar account menu, not the sidebar;
  // the route stays registered (and palette-searchable) but hidden from nav.
  { path: '/settings', label: 'Settings', group: 'Govern', element: () => <Settings />, hidden: true },
  // Invite-accept confirm screen: reached only via an invite link, never the
  // sidebar. The SAME component renders the pre-auth summary
  // (auth/preauthRouter.tsx); here authenticated=true selects the
  // confirm-join screen instead.
  { path: '/invite/accept', label: 'Accept invite', group: 'Govern', element: () => <AcceptInvite authenticated />, hidden: true, when: (c) => c.teams },
  // Operate: the instance-operator plane, visible only to a caller the
  // server has granted the admin capability (MITOS_CONSOLE_INSTANCE_ADMINS,
  // or the community-edition single-org-owner fallback). See
  // internal/saas/console/admin.go.
  { path: '/admin', label: 'Overview', group: 'Operate', element: () => <AdminOverview />, when: (c) => !!c.admin },
  { path: '/admin/orgs', label: 'Organizations', group: 'Operate', element: () => <AdminOrgs />, when: (c) => !!c.admin },
  { path: '/admin/nodes', label: 'Nodes', group: 'Operate', element: () => <AdminNodes />, when: (c) => !!c.admin },
  { path: '/admin/waitlist', label: 'Waitlist', group: 'Operate', element: () => <AdminWaitlist />, when: (c) => !!c.admin },
  { path: '/admin/audit', label: 'Audit', group: 'Operate', element: () => <AdminAudit />, when: (c) => !!c.admin },
]

export function visibleRoutes(caps: Capabilities): RouteDef[] {
  return ROUTES.filter((r) => !r.when || r.when(caps))
}

export function navRoutes(caps: Capabilities): RouteDef[] {
  return visibleRoutes(caps).filter((r) => !r.hidden)
}
