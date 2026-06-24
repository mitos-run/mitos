// Builds the TanStack Router from the capability-filtered routes. Intent
// preloading (hover / focus) is what makes navigation feel instant: the target
// route's component and data start loading before the click. The root route is
// a thin layout here; Task 5 replaces RootLayout's body with the real AppShell.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from '@tanstack/react-router'
import type { Capabilities } from './api'
import { visibleRoutes } from './nav/routes'

function RootLayout() {
  // Replaced by AppShell in Task 5. For now, render the active route directly so
  // the router is testable in isolation.
  return <Outlet />
}

export function createConsoleRouter(caps: Capabilities) {
  const rootRoute = createRootRoute({ component: RootLayout })
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
