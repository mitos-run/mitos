package controller_test

import (
	"testing"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
)

// TestBuildExposeRoutes exercises the pure route-set builder. It covers the
// four skip conditions and the happy path, plus the empty-Sharing default.
func TestBuildExposeRoutes(t *testing.T) {
	makeExpose := func(port int32, label, sharing string) *v1.SandboxExpose {
		return &v1.SandboxExpose{Port: port, Label: label, Sharing: sharing}
	}

	// Case (a): Ready sandbox with Expose, Endpoint, SandboxID, and a
	// resolvable token. This is the only one that should produce a route.
	sbReady := v1.Sandbox{}
	sbReady.Name = "sb-ready"
	sbReady.Spec.Expose = makeExpose(8080, "openclaw", "link")
	sbReady.Status.Phase = v1.SandboxReady
	sbReady.Status.Endpoint = "node1:9091"
	sbReady.Status.SandboxID = "sandbox-abc"

	// Case (b): Ready sandbox with Expose but the token lookup returns ok=false.
	sbNoToken := v1.Sandbox{}
	sbNoToken.Name = "sb-no-token"
	sbNoToken.Spec.Expose = makeExpose(3000, "notoken", "link")
	sbNoToken.Status.Phase = v1.SandboxReady
	sbNoToken.Status.Endpoint = "node2:9091"
	sbNoToken.Status.SandboxID = "sandbox-notoken"

	// Case (c): Not-Ready sandbox with Expose and Endpoint.
	sbNotReady := v1.Sandbox{}
	sbNotReady.Name = "sb-not-ready"
	sbNotReady.Spec.Expose = makeExpose(9000, "notready", "link")
	sbNotReady.Status.Phase = v1.SandboxPending
	sbNotReady.Status.Endpoint = "node3:9091"
	sbNotReady.Status.SandboxID = "sandbox-notready"

	// Case (d): Ready sandbox with no Expose set.
	sbNoExpose := v1.Sandbox{}
	sbNoExpose.Name = "sb-no-expose"
	sbNoExpose.Spec.Expose = nil
	sbNoExpose.Status.Phase = v1.SandboxReady
	sbNoExpose.Status.Endpoint = "node4:9091"
	sbNoExpose.Status.SandboxID = "sandbox-noexpose"

	// Case (e): Ready sandbox with Expose but empty Endpoint.
	sbNoEndpoint := v1.Sandbox{}
	sbNoEndpoint.Name = "sb-no-endpoint"
	sbNoEndpoint.Spec.Expose = makeExpose(4000, "noendpoint", "link")
	sbNoEndpoint.Status.Phase = v1.SandboxReady
	sbNoEndpoint.Status.Endpoint = ""
	sbNoEndpoint.Status.SandboxID = "sandbox-noendpoint"

	tokenFor := func(sb v1.Sandbox) (string, bool) {
		if sb.Name == sbNoToken.Name {
			return "", false
		}
		return "tok-" + sb.Status.SandboxID, true
	}

	sandboxes := []v1.Sandbox{sbReady, sbNoToken, sbNotReady, sbNoExpose, sbNoEndpoint}
	routes := controller.BuildExposeRoutes(sandboxes, tokenFor)

	if len(routes) != 1 {
		t.Fatalf("expected exactly 1 route, got %d", len(routes))
	}
	r := routes[0]
	if r.Label != "openclaw" {
		t.Errorf("Label: got %q, want %q", r.Label, "openclaw")
	}
	if r.SandboxID != "sandbox-abc" {
		t.Errorf("SandboxID: got %q, want %q", r.SandboxID, "sandbox-abc")
	}
	if r.NodeEndpoint != "node1:9091" {
		t.Errorf("NodeEndpoint: got %q, want %q", r.NodeEndpoint, "node1:9091")
	}
	if r.Port != 8080 {
		t.Errorf("Port: got %d, want %d", r.Port, 8080)
	}
	if r.Token != "tok-sandbox-abc" {
		t.Errorf("Token: got %q, want %q", r.Token, "tok-sandbox-abc")
	}
	if r.Sharing != "link" {
		t.Errorf("Sharing: got %q, want %q", r.Sharing, "link")
	}
	if !r.Ready {
		t.Errorf("Ready: got false, want true")
	}

	// Sub-case: empty Sharing defaults to "private".
	sbPrivate := v1.Sandbox{}
	sbPrivate.Name = "sb-private"
	sbPrivate.Spec.Expose = makeExpose(5000, "priv", "")
	sbPrivate.Status.Phase = v1.SandboxReady
	sbPrivate.Status.Endpoint = "node5:9091"
	sbPrivate.Status.SandboxID = "sandbox-private"

	routesPrivate := controller.BuildExposeRoutes([]v1.Sandbox{sbPrivate}, func(sb v1.Sandbox) (string, bool) {
		return "tok-priv", true
	})
	if len(routesPrivate) != 1 {
		t.Fatalf("default-sharing sub-case: expected 1 route, got %d", len(routesPrivate))
	}
	if routesPrivate[0].Sharing != "private" {
		t.Errorf("default-sharing sub-case: Sharing got %q, want %q", routesPrivate[0].Sharing, "private")
	}
}
