// Pre-auth route tree: mounted by App when capabilities returns 401.
// No AppShell - each route uses a minimal centered layout instead.
// /login and /signup are real pages; /verify is a placeholder.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  Navigate,
} from '@tanstack/react-router'
import { Login } from './Login'
import { Signup } from './Signup'
import { Verify } from './Verify'

function PreAuthShell() {
  return (
    <main
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        minHeight: '100vh',
        padding: 'var(--space-4)',
        background: 'var(--field)',
        color: 'var(--ink)',
      }}
    >
      <Outlet />
    </main>
  )
}


export function createPreAuthRouter(initialPath?: string) {
  const rootRoute = createRootRoute({ component: PreAuthShell })

  const loginRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/login',
    component: Login,
  })

  const signupRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/signup',
    component: Signup,
  })

  const verifyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/verify',
    component: Verify,
  })

  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => <Navigate to="/login" />,
  })

  const routeTree = rootRoute.addChildren([indexRoute, loginRoute, signupRoute, verifyRoute])

  const router = createRouter({
    routeTree,
    defaultPreload: 'intent',
    defaultPreloadStaleTime: 0,
    defaultNotFoundComponent: () => <Navigate to="/login" />,
  })

  if (initialPath) {
    void router.navigate({ to: initialPath })
  }

  return router
}
