package mitos

// Workspace.Serve: expose a workspace-bound sandbox over HTTPS and return a
// handle carrying the public URL. This is the Go SDK side of issue #312
// (Expose slice 5a).
//
// The implementation builds a Sandbox CRD directly (bypassing the generic
// Create helper so spec.expose can be set in the same POST body as
// spec.workspaceRef and spec.source.poolRef). It then polls until Ready.
//
// Token minting is a follow-up: the per-sandbox bearer token is intentionally
// not set on the expose route here; the proxy enforces the sharing tier
// independently.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// reservedExposeLabels mirrors internal/preview.reservedLabels so the SDK can
// validate labels without importing internal/. Keep this list consistent with
// the proxy (internal/preview/route.go reservedLabels map).
var reservedExposeLabels = map[string]struct{}{
	"www": {}, "app": {}, "api": {}, "console": {}, "gateway": {},
	"admin": {}, "auth": {}, "login": {}, "account": {}, "mail": {},
	"static": {}, "assets": {}, "cdn": {}, "status": {},
}

// exposeLabelRE matches a valid single DNS label: starts and ends with
// alphanumeric, may contain hyphens in the middle, max 63 characters.
var exposeLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// buildExposeURL validates label and exposeDomain and returns the HTTPS expose
// URL. It normalizes label to lowercase before validation. It is the SDK-local
// equivalent of internal/agentcli.BuildExposeURL; the SDK must not import
// internal/.
func buildExposeURL(label, exposeDomain string) (string, error) {
	label = strings.ToLower(label)

	if exposeDomain == "" {
		return "", &Error{
			Code:        "missing_expose_domain",
			Message:     "expose domain is required",
			Cause:       "no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
			Remediation: "Pass WithServeExposeDomain(domain) or set the MITOS_EXPOSE_DOMAIN environment variable.",
		}
	}
	if label == "" {
		return "", &Error{
			Code:        "invalid_expose_label",
			Message:     "expose label is required",
			Cause:       "label is empty",
			Remediation: "Pass WithServeLabel(name) or use a sandbox name that is a valid single DNS label.",
		}
	}
	if len(label) > 63 {
		return "", &Error{
			Code:        "invalid_expose_label",
			Message:     fmt.Sprintf("expose label %q exceeds 63 characters", label),
			Cause:       fmt.Sprintf("label length %d > 63", len(label)),
			Remediation: "Use a shorter label (at most 63 characters).",
		}
	}
	if !exposeLabelRE.MatchString(label) {
		return "", &Error{
			Code:        "invalid_expose_label",
			Message:     fmt.Sprintf("expose label %q is not a valid single DNS label", label),
			Cause:       "label must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$",
			Remediation: "Use only lowercase letters, digits, and hyphens; do not start or end with a hyphen.",
		}
	}
	if _, reserved := reservedExposeLabels[label]; reserved {
		return "", &Error{
			Code:        "reserved_expose_label",
			Message:     fmt.Sprintf("expose label %q is reserved and may not be used by tenants", label),
			Cause:       fmt.Sprintf("label %q is in the reserved set", label),
			Remediation: "Choose a different label that is not a well-known control-plane name.",
		}
	}
	return "https://" + label + "." + exposeDomain + "/", nil
}

// ServeOption configures Workspace.Serve.
type ServeOption func(*serveConfig)

type serveConfig struct {
	pool         string
	port         int
	sharing      string
	label        string
	exposeDomain string
}

// WithServePool sets the SandboxPool to claim from. Required.
func WithServePool(pool string) ServeOption {
	return func(c *serveConfig) { c.pool = pool }
}

// WithServePort sets the guest TCP port to expose. Defaults to 8080.
func WithServePort(port int) ServeOption {
	return func(c *serveConfig) { c.port = port }
}

// WithServeSharing sets the access tier ("private", "link", "org",
// "authenticated", "public"). Defaults to "private".
func WithServeSharing(sharing string) ServeOption {
	return func(c *serveConfig) { c.sharing = sharing }
}

// WithServeLabel sets an explicit subdomain label. When omitted the sandbox
// name is used as the label.
func WithServeLabel(label string) ServeOption {
	return func(c *serveConfig) { c.label = label }
}

// WithServeExposeDomain sets the base expose domain (for example "mitos.app").
// When omitted the MITOS_EXPOSE_DOMAIN environment variable is used.
func WithServeExposeDomain(domain string) ServeOption {
	return func(c *serveConfig) { c.exposeDomain = domain }
}

// ServedWorkspace is the handle returned by Workspace.Serve. It carries the
// public URL (#312 deliverable) and the identity of the backing sandbox.
type ServedWorkspace struct {
	// SandboxName is the name of the Sandbox CRD that backs this serve session.
	SandboxName string
	// Label is the single DNS label used in the URL subdomain.
	Label string
	// URL is the public HTTPS URL: https://<label>.<exposeDomain>/.
	URL string
	// Sharing is the effective access tier ("private", "link", etc.).
	Sharing string
}

// serveWaitInterval is the polling interval used while waiting for the sandbox
// to reach Ready. It is a variable so tests can override it to avoid sleeping.
var serveWaitInterval = 500 * time.Millisecond

// Serve creates a Sandbox bound to this workspace with spec.expose set, then
// polls until the sandbox reaches Ready. It returns a *ServedWorkspace carrying
// the public HTTPS URL.
//
// Options: WithServePool (required), WithServePort (default 8080),
// WithServeSharing (default "private"), WithServeLabel (default: sandbox name),
// WithServeExposeDomain (default: MITOS_EXPOSE_DOMAIN env var).
func (w *Workspace) Serve(ctx context.Context, opts ...ServeOption) (*ServedWorkspace, error) {
	cfg := serveConfig{
		port:    8080,
		sharing: "private",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.pool == "" {
		return nil, &Error{
			Code:        "missing_serve_pool",
			Message:     "Serve needs a pool",
			Cause:       "WithServePool was not provided",
			Remediation: "Pass WithServePool(name) to select the SandboxPool to claim from.",
		}
	}
	if cfg.port < 1 || cfg.port > 65535 {
		return nil, &Error{
			Code:        "invalid_serve_port",
			Message:     "Serve port out of range",
			Cause:       fmt.Sprintf("port %d is not in 1-65535", cfg.port),
			Remediation: "Pass WithServePort(n) with a port in the range 1-65535.",
		}
	}

	// Resolve expose domain: option first, then env var.
	exposeDomain := cfg.exposeDomain
	if exposeDomain == "" {
		exposeDomain = os.Getenv("MITOS_EXPOSE_DOMAIN")
	}
	if exposeDomain == "" {
		return nil, &Error{
			Code:        "missing_expose_domain",
			Message:     "expose domain is required",
			Cause:       "no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
			Remediation: "Pass WithServeExposeDomain(domain) or set the MITOS_EXPOSE_DOMAIN environment variable.",
		}
	}

	// Generate the sandbox name up front so we can use it as the default label
	// before we know what the server will assign (we control the name).
	sbName := "sandbox-" + randomHex(4)

	// Determine the effective label now; if not explicit, use the sandbox name.
	label := cfg.label
	if label == "" {
		label = sbName
	}

	// Validate and construct the URL before sending anything to the cluster so
	// a bad label fails fast without leaving a partially configured sandbox.
	url, err := buildExposeURL(label, exposeDomain)
	if err != nil {
		return nil, err
	}

	// Build the Sandbox CRD body with spec.expose included in the initial POST.
	// This matches the api/v1 SandboxExpose JSON shape: port, label, sharing.
	// The new policy fields (network, forwardAuthURL, allowedPrincipals,
	// allowedEmailDomains) are optional and omitted here.
	spec := map[string]any{
		"source":       map[string]any{"poolRef": map[string]any{"name": cfg.pool}},
		"workspaceRef": map[string]any{"name": w.Name},
		"expose": map[string]any{
			"port":    cfg.port,
			"label":   label,
			"sharing": cfg.sharing,
		},
	}
	body := k8sObject{
		"apiVersion": k8sAPIGroup + "/" + k8sAPIVersion,
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": sbName, "namespace": w.Namespace},
		"spec":       spec,
	}
	if _, err := w.agent.k8s.createObject(ctx, w.Namespace, "sandboxes", body); err != nil {
		return nil, fmt.Errorf("Serve: create sandbox: %w", err)
	}

	// Wait until the sandbox reaches Ready (or the context is cancelled).
	if err := w.waitSandboxReady(ctx, sbName); err != nil {
		return nil, fmt.Errorf("Serve: wait ready: %w", err)
	}

	return &ServedWorkspace{
		SandboxName: sbName,
		Label:       label,
		URL:         url,
		Sharing:     cfg.sharing,
	}, nil
}

// waitSandboxReady polls the Sandbox until it reaches PhaseReady or the context
// is cancelled. A Failed phase is returned as an error immediately.
func (w *Workspace) waitSandboxReady(ctx context.Context, name string) error {
	for {
		obj, err := w.agent.k8s.getObject(ctx, w.Namespace, "sandboxes", name)
		if err != nil {
			return err
		}
		phase := SandboxPhase(statusPhase(obj))
		switch phase {
		case PhaseReady:
			return nil
		case PhaseFailed:
			return &Error{
				Code:        "sandbox_failed",
				Message:     fmt.Sprintf("sandbox %s reached Failed phase", name),
				Cause:       "the controller reported a Failed phase before Ready",
				Remediation: "Check the Sandbox status for more detail (kubectl describe sandbox " + name + ").",
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serveWaitInterval):
		}
	}
}
