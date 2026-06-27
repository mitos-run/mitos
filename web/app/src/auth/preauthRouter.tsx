// Pre-auth route tree: mounted by App when capabilities returns 401.
// No AppShell - each route uses a minimal centered layout instead.
// /login uses the real Login page; /signup and /verify are placeholders.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  Navigate,
} from '@tanstack/react-router'
import { Login } from './Login'

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

function SignupPlaceholder() {
  return (
    <div style={{ textAlign: 'center' }}>
      <h1>Create your account</h1>
    </div>
  )
}

function VerifyPlaceholder() {
  return (
    <div style={{ textAlign: 'center' }}>
      <h1>Check your email</h1>
    </div>
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
    component: SignupPlaceholder,
  })

  const verifyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/verify',
    component: VerifyPlaceholder,
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
