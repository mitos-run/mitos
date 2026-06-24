// The single source of truth for the console's information architecture. Both
// the nav (AppShell) and the router (router.tsx) derive from this array, so a
// route is declared exactly once. `when` gates a route on the server-advertised
// capabilities document; a route with no `when` is always present.
import type { Capabilities } from '../api'
import { Instruments } from '../views/Instruments'
import { Sandboxes } from '../views/Sandboxes'
import { ForkTree } from '../views/forktree/ForkTree'
import { Secrets } from '../views/Secrets'
import { Placeholder } from '../views/Placeholder'

export type NavGroupName = 'Run' | 'Build' | 'Govern' | 'Settings'
export const GROUP_ORDER: NavGroupName[] = ['Run', 'Build', 'Govern', 'Settings']

export type RouteDef = {
  path: string
  label: string
  group: NavGroupName
  element: () => JSX.Element
  when?: (c: Capabilities) => boolean
}

export const ROUTES: RouteDef[] = [
  { path: '/', label: 'Instruments', group: 'Run', element: () => <Instruments />, when: (c) => c.proof },
  { path: '/sandboxes', label: 'Sandboxes', group: 'Run', element: () => <Sandboxes /> },
  { path: '/forks', label: 'Fork tree', group: 'Run', element: () => <ForkTree /> },
  { path: '/workspaces', label: 'Workspaces', group: 'Build', element: () => <Placeholder title="Workspaces" endpoint="/console/workspaces" phase="B2" /> },
  { path: '/templates', label: 'Templates', group: 'Build', element: () => <Placeholder title="Templates" endpoint="/console/templates" phase="B2" /> },
  { path: '/secrets', label: 'Secrets', group: 'Build', element: () => <Secrets /> },
  { path: '/keys', label: 'API keys', group: 'Build', element: () => <Placeholder title="API keys" endpoint="/console/keys" phase="B2" /> },
  { path: '/members', label: 'Members', group: 'Govern', element: () => <Placeholder title="Members & roles" endpoint="/console/members" phase="B2" />, when: (c) => c.teams },
  { path: '/audit', label: 'Audit', group: 'Govern', element: () => <Placeholder title="Audit log" endpoint="/console/audit" phase="B2" /> },
  { path: '/usage', label: 'Usage', group: 'Govern', element: () => <Placeholder title="Usage & cost" endpoint="/console/usage" phase="B2" /> },
  { path: '/billing', label: 'Billing', group: 'Govern', element: () => <Placeholder title="Billing" endpoint="/console/billing" phase="B2" />, when: (c) => c.billing },
  { path: '/settings', label: 'Settings', group: 'Settings', element: () => <Placeholder title="Settings" endpoint="/console/capabilities" phase="B2" /> },
]

export function visibleRoutes(caps: Capabilities): RouteDef[] {
  return ROUTES.filter((r) => !r.when || r.when(caps))
}
