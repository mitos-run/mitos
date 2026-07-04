package controlplane

// Tests for the terminal-phase gate on the runtime proxy paths (issue #688
// follow-up): a sandbox whose claim is Terminated keeps its stale
// Status.Endpoint (both run modes do; the VM is stopped and, in husk mode, the
// pod deleted), so exec/files/run_code/PTY calls between lifetime expiry and
// object deletion used to dial a dead endpoint and surface a generic 502
// internal error. docs/lifecycle.md promises the typed idle_timeout error for
// a reaped sandbox (the API v2 LLM-legible error rule), and the gate reads the
// claim PHASE, upstream of any mode-specific endpoint, so raw-forkd and
// husk-backed sandboxes get the same typed answer.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// lifetimeTerminatedSandbox builds a sandbox exactly as the controller's
// terminateLifetime stamps it: phase Terminated, FinishedAt, and a Terminated
// condition carrying the reap reason, with the stale runtime endpoint still in
// status.
func lifetimeTerminatedSandbox(org, name, endpoint, token, reason string) (*v1.Sandbox, *metav1.Time) {
	sb, secret := readySandbox(org, name, endpoint, token)
	_ = secret
	now := metav1.Now()
	sb.Status.Phase = v1.SandboxTerminated
	sb.Status.FinishedAt = &now
	sb.Status.Conditions = []metav1.Condition{{
		Type:               "Terminated",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            "lifetime bound exceeded",
	}}
	return sb, &now
}

// TestProxyTerminatedSandboxTypedIdleTimeoutNeverDialed asserts a runtime call
// against a lifetime-expired sandbox returns the documented typed idle_timeout
// error (410 Gone) and NEVER dials the stale endpoint, for each reap reason
// terminateLifetime records.
func TestProxyTerminatedSandboxTypedIdleTimeoutNeverDialed(t *testing.T) {
	for _, reason := range []string{"IdleTimeout", "MaxLifetimeExceeded", "TimeoutExpired"} {
		t.Run(reason, func(t *testing.T) {
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			endpoint := strings.TrimPrefix(srv.URL, "http://")

			sb, _ := lifetimeTerminatedSandbox(orgA, "sb-gone", endpoint, "tok", reason)
			_, secret := readySandbox(orgA, "sb-gone", endpoint, "tok")
			cp := New(newFakeClient(t, sb, secret), WithHTTPClient(srv.Client()))

			hdr := http.Header{}
			hdr.Set("X-Sandbox-Id", "sb-gone")
			resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
				OrgID: orgA, Op: "sandbox.runtime", Path: "/sandbox.v1.Sandbox/Exec",
				Method: http.MethodPost, Header: hdr, BodyStream: strings.NewReader("{}"),
			})
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			if reached {
				t.Fatal("runtime call against a Terminated sandbox DIALED the stale endpoint")
			}
			if resp.Status != http.StatusGone {
				t.Fatalf("status = %d, want 410 Gone (the typed reaped-sandbox error), body %s", resp.Status, resp.Body)
			}
			body := decodeBody(t, resp.Body)
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error envelope in %s", resp.Body)
			}
			if errObj["code"] != string(apierr.CodeIdleTimeout) {
				t.Errorf("code = %v, want idle_timeout (docs/lifecycle.md: a reaped sandbox returns the typed idle_timeout error, not a generic 502)", errObj["code"])
			}
			if rem, _ := errObj["remediation"].(string); rem == "" {
				t.Error("remediation is empty; the error must be actionable (API v2 LLM-legible rule)")
			}
			if cause, _ := errObj["cause"].(string); !strings.Contains(cause, reason) {
				t.Errorf("cause %q does not carry the termination reason %q", cause, reason)
			}
		})
	}
}

// TestProxyFailedSandboxTypedNeverDialed asserts a runtime call against a
// terminally Failed sandbox (raw-forkd node loss stamps Failed and keeps the
// stale endpoint) gets a typed, actionable answer and never dials the dead
// endpoint.
func TestProxyFailedSandboxTypedNeverDialed(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	sb, secret := readySandbox(orgA, "sb-dead", endpoint, "tok")
	now := metav1.Now()
	sb.Status.Phase = v1.SandboxFailed
	sb.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "NodeLost",
		Message:            "the node running this sandbox left the cluster",
	}}
	cp := New(newFakeClient(t, sb, secret), WithHTTPClient(srv.Client()))

	hdr := http.Header{}
	hdr.Set("X-Sandbox-Id", "sb-dead")
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/sandbox.v1.Sandbox/Exec",
		Method: http.MethodPost, Header: hdr, BodyStream: strings.NewReader("{}"),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if reached {
		t.Fatal("runtime call against a Failed sandbox DIALED the stale endpoint")
	}
	if resp.Status == http.StatusBadGateway || resp.Status == http.StatusInternalServerError {
		t.Fatalf("status = %d; a Failed sandbox must get a typed answer, not a generic internal error", resp.Status)
	}
	body := decodeBody(t, resp.Body)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %s", resp.Body)
	}
	if errObj["code"] != string(apierr.CodeNotFound) {
		t.Errorf("code = %v, want not_found (the sandbox is not running and never will be)", errObj["code"])
	}
	if cause, _ := errObj["cause"].(string); !strings.Contains(cause, "node running this sandbox left") {
		t.Errorf("cause %q does not carry the failure detail from the Ready condition", cause)
	}
}

// TestResolveRuntimeTerminatedIsTypedIdleTimeout asserts the PTY WebSocket
// resolve path refuses a Terminated sandbox with the same typed error as the
// Forward proxy, instead of resolving the stale endpoint.
func TestResolveRuntimeTerminatedIsTypedIdleTimeout(t *testing.T) {
	sb, _ := lifetimeTerminatedSandbox(orgA, "sb-pty-gone", "10.1.2.3:9091", "tok", "IdleTimeout")
	_, secret := readySandbox(orgA, "sb-pty-gone", "10.1.2.3:9091", "tok")
	cp := New(newFakeClient(t, sb, secret))

	_, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-pty-gone")
	if aerr == nil {
		t.Fatal("ResolveRuntime resolved a Terminated sandbox; the PTY would dial a dead endpoint")
	}
	if aerr.Code != string(apierr.CodeIdleTimeout) {
		t.Fatalf("code = %q, want idle_timeout", aerr.Code)
	}
	if aerr.Status != http.StatusGone {
		t.Errorf("status = %d, want 410", aerr.Status)
	}
}
