// Builds the TanStack Router from the capability-filtered routes. Intent
// preloading (hover / focus) warms the target route's component before the
// click; data is fetched by each view's own hook after navigation. The root
// route uses AppShell so every route is wrapped in the grouped sidebar and
// ownership badge.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Navigate,
} from '@tanstack/react-router'
import type { Capabilities } from './api'
import { visibleRoutes } from './nav/routes'
import { AppShell } from './nav/AppShell'

export function createConsoleRouter(caps: Capabilities) {
  const visible = visibleRoutes(caps)
  const fallbackPath = visible[0]?.path ?? '/sandboxes'

  const rootRoute = createRootRoute({ component: AppShell })
  const children = visible.map((r) => {
    const Element = r.element
    return createRoute({
      getParentRoute: () => rootRoute,
      path: r.path,
      component: () => <Element />,
    })
  })
  const routeTree = rootRoute.addChildren(children)
  return createRouter({
    routeTree,
    defaultPreload: 'intent',
    defaultPreloadStaleTime: 0, // let TanStack Query own caching
    defaultNotFoundComponent: () => <Navigate to={fallbackPath} />,
  })
}
