// Package controlplane is the real hosted control plane behind the public
// gateway (issue #210, ROADMAP SaaS P1). It turns an authenticated, org-scoped
// ForwardRequest into real Kubernetes actions on the mitos.run/v1 Sandbox kind
// and reverse-proxies runtime calls (exec, files, run_code over the Connect
// sandbox.v1.Sandbox service) to the sandbox endpoint with the per-sandbox
// bearer token.
//
// Security: this is the cross-tenant boundary. The org id is taken ONLY from
// ForwardRequest.OrgID (the gateway verified it from the customer key); a
// customer can never influence it. Every Get, List, Delete, and proxy is scoped
// to namespaceForOrg(orgID) AND re-checks the mitos.run/org label on the object,
// so a request that names another org's sandbox id returns not_found and never
// mutates or reaches it. The per-sandbox token is read from the controller-owned
// Secret and is returned to the caller ONLY on create (so the SDK can address the
// sandbox directly); it is never logged and never placed in an error.
//
// Single-tenant mode: when WithSingleTenantNamespace is set all namespace
// derivation is pinned to one fixed namespace (the shared pool namespace).
// Org-label authz is still enforced in that mode: a key for org B cannot read
// or mutate a sandbox owned by org A, even though both share the namespace.
package controlplane

import (
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/internal/tenant"
)

// Defaults for the readiness wait. A create call blocks until the sandbox is
// Ready, Failed, or the timeout elapses; these bound that wait. Readiness is
// normally observed by a WATCH (readywatch.go); the poll interval governs only
// the fail-open fallback loop, and is kept small so a fallback create is never
// quantized to a coarse tick boundary.
const (
	defaultReadyTimeout = 120 * time.Second
	defaultPollInterval = 25 * time.Millisecond
)

// K8sControlPlane is the real ControlPlane. It holds a controller-runtime client
// (the lifecycle path), an http.Client (the runtime reverse proxy), and config.
type K8sControlPlane struct {
	c            client.Client
	httpClient   *http.Client
	readyTimeout time.Duration
	pollInterval time.Duration
	// defaultPool is the pool a create request falls back to when it names neither
	// a pool nor an image. Empty means a create without a pool is rejected.
	defaultPool string
	// singleTenantNamespace, when non-empty, pins all sandbox operations to this
	// fixed namespace instead of the per-org mitos-org-<id> namespace. Use only
	// for QA deployments where per-org namespaces are not provisioned. Org-label
	// authz is still enforced: sandboxes still carry the org label and cross-org
	// checks still apply.
	singleTenantNamespace string
	now                   func() time.Time

	// poolSeen caches POSITIVE pool-existence pre-check results per
	// namespace/name until the stored expiry, so a repeat create of a stable
	// hosted pool skips the serialized apiserver Get (see poolCheckTTL in
	// forward.go). Absence is never stored.
	poolSeenMu sync.Mutex
	poolSeen   map[string]time.Time
}

// Option configures a K8sControlPlane.
type Option func(*K8sControlPlane)

// WithReadyTimeout sets the create readiness poll ceiling. A non-positive value
// is ignored (the default stands).
func WithReadyTimeout(d time.Duration) Option {
	return func(k *K8sControlPlane) {
		if d > 0 {
			k.readyTimeout = d
		}
	}
}

// WithPollInterval sets the create readiness poll interval. A non-positive value
// is ignored.
func WithPollInterval(d time.Duration) Option {
	return func(k *K8sControlPlane) {
		if d > 0 {
			k.pollInterval = d
		}
	}
}

// WithDefaultPool sets the fallback pool name used when a create request names
// neither a pool nor an image.
func WithDefaultPool(name string) Option {
	return func(k *K8sControlPlane) { k.defaultPool = name }
}

// WithHTTPClient overrides the runtime-proxy HTTP client (tests inject one that
// targets an httptest.Server).
func WithHTTPClient(h *http.Client) Option {
	return func(k *K8sControlPlane) {
		if h != nil {
			k.httpClient = h
		}
	}
}

// WithSingleTenantNamespace pins all sandbox operations to ns instead of the
// per-org mitos-org-<id> namespace. When ns is empty the option is a no-op
// and per-org namespacing applies. Use this ONLY for single-tenant QA
// deployments where per-org namespaces are not provisioned: the shared pool
// namespace (e.g. "mitos") already contains the SandboxPool and forkd can
// reach it.
//
// Sandbox LIFECYCLE authz is preserved: sandboxes carry the org label and a key
// for org B cannot read, exec in, or terminate org A's sandbox even in a shared
// namespace (getOwned re-checks the org label on every id-taking op).
//
// The shared namespace does NOT provide an org boundary for objects a create
// REFERENCES by bare name: a Secret named via secretRef or a Workspace named via
// workspace has no per-org meaning when every org shares the namespace, so a
// tenant could name a platform Secret or another tenant's object and have it
// mounted (GHSA-pgv2-9w24-j7wh). The create path therefore REFUSES secretRef and
// workspace references while single-tenant mode is set (see create in
// forward.go), and the controller resolvers additionally refuse a referenced
// object whose org label differs from the claim's. Use per-org tenancy (a
// namespace per org) to enable secretRef and workspaces safely.
func WithSingleTenantNamespace(ns string) Option {
	return func(k *K8sControlPlane) {
		if ns != "" {
			k.singleTenantNamespace = ns
		}
	}
}

// namespaceForOrg returns the sandbox namespace for orgID. In single-tenant
// mode (WithSingleTenantNamespace set) it returns the fixed namespace for all
// orgs. Otherwise it delegates to tenant.NamespaceForOrg.
func (k *K8sControlPlane) namespaceForOrg(orgID string) string {
	if k.singleTenantNamespace != "" {
		return k.singleTenantNamespace
	}
	return tenant.NamespaceForOrg(orgID)
}

// New builds a K8sControlPlane over the given controller-runtime client. The
// client's scheme MUST have mitos.run/v1 and corev1 registered so the control
// plane can read Sandboxes and the per-sandbox token Secret.
func New(c client.Client, opts ...Option) *K8sControlPlane {
	k := &K8sControlPlane{
		c:            c,
		httpClient:   &http.Client{Timeout: 0}, // no overall timeout: streams are long-lived.
		readyTimeout: defaultReadyTimeout,
		pollInterval: defaultPollInterval,
		now:          time.Now,
	}
	for _, o := range opts {
		o(k)
	}
	return k
}
