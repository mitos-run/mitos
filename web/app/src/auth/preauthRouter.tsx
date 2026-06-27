// Pre-auth route tree: mounted by App when capabilities returns 401.
// No AppShell - each route uses a minimal centered layout instead.
// /login, /signup, /verify are placeholders; real pages land in later tasks.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  Navigate,
} from '@tanstack/react-router'

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

function LoginPlaceholder() {
  return (
    <div style={{ textAlign: 'center' }}>
      <h1 style={{ marginBottom: 'var(--space-4)' }}>Sign in to Mitos</h1>
      <button type="button">Continue with GitHub</button>
    </div>
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
    component: LoginPlaceholder,
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
  })

  if (initialPath) {
    void router.navigate({ to: initialPath })
  }

  return router
}
