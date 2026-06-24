// Builds the TanStack Router from the capability-filtered routes. Intent
// preloading (hover / focus) is what makes navigation feel instant: the target
// route's component and data start loading before the click. The root route uses
// AppShell so every route is wrapped in the grouped sidebar and ownership badge.
import {
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import type { Capabilities } from './api'
import { visibleRoutes } from './nav/routes'
import { AppShell } from './nav/AppShell'

export function createConsoleRouter(caps: Capabilities) {
  const rootRoute = createRootRoute({ component: AppShell })
  const children = visibleRoutes(caps).map((r) => {
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
  })
}
