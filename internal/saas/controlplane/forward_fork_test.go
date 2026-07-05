package controlplane

// Fork readiness discovery (per the hosted-live-fork plan, Step 1):
//
//	(a) readiness signal: the fork Sandbox's top-level Status.Phase reaches
//	    Ready (the fork engine flips it once its children are up).
//	(b) child endpoint: the fork object itself never carries Status.Endpoint;
//	    the controller stamps Status.Children[0].Endpoint, which
//	    runtimeEndpoint resolves.
//	(c) token Secret: per CHILD, "<child>-sandbox-token" (reissued; the
//	    source's token never opens a fork), resolved by tokenSecretNameFor.
//	(d) startup latency: Status.Children[0].StartupLatencyMs (the engine
//	    measurement), read by forkTimeMs with a wall-clock fallback.
//
// flipForkToReadyWhenCreated (fork_test.go) mimics exactly those fields.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// TestForkHonorsRequestedChildID asserts a fork body naming a valid child id
// creates the fork Sandbox under exactly that name and echoes it as the
// response id, so the SDK's fork(id=...) contract holds on the hosted API.
func TestForkHonorsRequestedChildID(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	stop := flipForkToReadyWhenCreated(t, c, orgA, "10.9.9.9:9091", "child-token", 7)
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"id":"child-1","pause_source":true}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	if got := decodeBody(t, resp.Body)["id"]; got != "child-1" {
		t.Errorf("response id = %v, want child-1 (the requested child id must be honored)", got)
	}
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: "child-1"}, &sb); err != nil {
		t.Fatalf("get fork child-1: %v", err)
	}
	if sb.Spec.Source.FromSandbox == nil || sb.Spec.Source.FromSandbox.Name != "sb-src" {
		t.Errorf("source = %+v, want fromSandbox naming sb-src", sb.Spec.Source)
	}
}

// TestForkInvalidChildIDIsInvalidInput asserts a child id that is not a valid
// DNS-1123 name is an INSTANT 400 invalid_input with remediation, before any
// object is created: the api server would reject it anyway, but the typed
// pre-check names the rule instead of leaking a raw validation error.
func TestForkInvalidChildIDIsInvalidInput(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"id":"Bad_ID!"}`),
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "invalid_input") {
		t.Errorf("error is not typed invalid_input: %s", body)
	}
	if !strings.Contains(body, "remediation") {
		t.Errorf("error carries no remediation: %s", body)
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Errorf("created %d fork objects for an invalid child id", len(forks))
	}
}

// TestForkInvalidSecretInheritanceModeIsInvalidInput asserts an unknown
// secret_inheritance value is a 400 naming the two modes, never a silently
// dropped field that would default-deny with a confusing later rejection.
func TestForkInvalidSecretInheritanceModeIsInvalidInput(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"secret_inheritance":"clone"}`),
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "invalid_input") || !strings.Contains(body, "reissue") || !strings.Contains(body, "inherit") {
		t.Errorf("error must be typed invalid_input naming both modes: %s", body)
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Errorf("created %d fork objects for an invalid secret_inheritance", len(forks))
	}
}

// TestForkSecretInheritanceDeniedSurfacesForbidden asserts the controller's
// default-deny secrets gate (the Rejected/SecretInheritanceDenied condition)
// surfaces as a 403 forbidden whose remediation names the exact wire-level
// opt-in ("secret_inheritance": "inherit"), so an LLM agent can self-correct
// without reading controller source.
func TestForkSecretInheritanceDeniedSurfacesForbidden(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	src.Spec.Secrets = []v1.SecretMount{{Name: "api-key"}}
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	// The flipper stamps EXACTLY what the fork controller records on the
	// default-deny path (sandboxfork_controller.go): Rejected with reason
	// SecretInheritanceDenied and no Failed phase.
	stop := flipWhenCreated(t, c, orgA, func(sb *v1.Sandbox) {
		sb.Status.Conditions = []metav1.Condition{{
			Type: "Rejected", Status: metav1.ConditionTrue, Reason: "SecretInheritanceDenied",
			Message:            "source sandbox holds secrets; recreate the fork with spec.secretInheritance=inherit to permit it (forks duplicate guest memory, including secret values)",
			LastTransitionTime: metav1.Now(),
		}}
	})
	defer stop()

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork", Body: []byte(`{}`),
	})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "forbidden") {
		t.Errorf("error is not typed forbidden: %s", body)
	}
	if !strings.Contains(body, `\"secret_inheritance\": \"inherit\"`) {
		t.Errorf("remediation does not name the wire-level opt-in: %s", body)
	}
}

// TestForkSecretInheritanceOptInPassesThrough asserts the explicit opt-in
// ("secret_inheritance":"inherit") lands on the built object's
// Spec.SecretInheritance so the controller's audit trail
// (SecretInheritance/ExplicitOptIn) and gate see the caller's decision.
func TestForkSecretInheritanceOptInPassesThrough(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	src.Spec.Secrets = []v1.SecretMount{{Name: "api-key"}}
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	stop := flipForkToReadyWhenCreated(t, c, orgA, "10.9.9.9:9091", "child-token", 5)
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"secret_inheritance":"inherit"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	name, _ := decodeBody(t, resp.Body)["id"].(string)
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: name}, &sb); err != nil {
		t.Fatalf("get fork %q: %v", name, err)
	}
	if sb.Spec.SecretInheritance != v1.SecretInherit {
		t.Errorf("SecretInheritance = %q, want %q (the opt-in must pass through)", sb.Spec.SecretInheritance, v1.SecretInherit)
	}
}

// TestForkResponseContract pins the full SDK response contract: 201 with id,
// endpoint, token, phase, template_id (the SOURCE's pool), and fork_time_ms as
// a number (the Python DirectSandbox constructor raises KeyError on any
// missing key), plus the X-Mitos-Pool header the gateway telemetry reads.
func TestForkResponseContract(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	stop := flipForkToReadyWhenCreated(t, c, orgA, "10.9.9.9:9091", "child-token", 33)
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"pause_source":true}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	for _, key := range []string{"id", "endpoint", "token", "phase", "template_id", "fork_time_ms"} {
		if _, ok := m[key]; !ok {
			t.Errorf("response is missing %q (the SDK raises KeyError without it): %s", key, resp.Body)
		}
	}
	if _, ok := m["fork_time_ms"].(float64); !ok {
		t.Errorf("fork_time_ms = %T, want a JSON number", m["fork_time_ms"])
	}
	if m["template_id"] != "default" {
		t.Errorf("template_id = %v, want default (the source's pool)", m["template_id"])
	}
	if got := resp.Header.Get("X-Mitos-Pool"); got != "default" {
		t.Errorf("X-Mitos-Pool = %q, want default (the gateway telemetry reads it)", got)
	}
}
