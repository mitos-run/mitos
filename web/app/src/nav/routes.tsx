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

export type NavGroupName = 'Run' | 'Build' | 'Govern' | 'Billing'
export const GROUP_ORDER: NavGroupName[] = ['Run', 'Build', 'Govern', 'Billing']

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
  { path: '/workspaces', label: 'Workspaces', group: 'Build', element: () => <Placeholder title="Workspaces" endpoint="/console/workspaces" phase="B2" /> },
  { path: '/templates', label: 'Templates', group: 'Build', element: () => <Templates /> },
  { path: '/secrets', label: 'Secrets', group: 'Build', element: () => <Secrets /> },
  { path: '/keys', label: 'API keys', group: 'Build', element: () => <Keys /> },
  { path: '/members', label: 'Members', group: 'Govern', element: () => <Members />, when: (c) => c.teams },
  { path: '/projects', label: 'Projects', group: 'Govern', element: () => <Projects />, when: (c) => c.teams },
  { path: '/audit', label: 'Audit', group: 'Govern', element: () => <Audit /> },
  { path: '/retention', label: 'Data and retention', group: 'Govern', element: () => <Retention /> },
  { path: '/usage', label: 'Usage', group: 'Billing', element: () => <Usage /> },
  { path: '/billing', label: 'Billing', group: 'Billing', element: () => <Billing />, when: (c) => c.billing },
  // Account settings is reached from the top-bar account menu, not the sidebar;
  // the route stays registered (and palette-searchable) but hidden from nav.
  { path: '/settings', label: 'Settings', group: 'Govern', element: () => <Settings />, hidden: true },
]

export function visibleRoutes(caps: Capabilities): RouteDef[] {
  return ROUTES.filter((r) => !r.when || r.when(caps))
}

export function navRoutes(caps: Capabilities): RouteDef[] {
  return visibleRoutes(caps).filter((r) => !r.hidden)
}
