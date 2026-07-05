package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// flipForkToReadyWhenCreated watches the org namespace for a newly created
// fromSandbox fork and flips it to Ready the way the controller's fork engine
// does: the fork object itself never gets Status.Endpoint; instead its first
// child slot ("<name>-fork-0") records the endpoint plus the engine-measured
// StartupLatencyMs, and the per-CHILD token Secret
// ("<name>-fork-0-sandbox-token") is written.
func flipForkToReadyWhenCreated(t *testing.T, c client.Client, org, endpoint, token string, latencyMs int64) (stop func()) {
	t.Helper()
	return flipWhenCreated(t, c, org, func(sb *v1.Sandbox) {
		child := sb.Name + "-fork-0"
		sb.Status.Phase = v1.SandboxReady
		sb.Status.ReadyReplicas = 1
		sb.Status.Children = []v1.SandboxChild{{
			Name:             child,
			SandboxID:        child,
			Endpoint:         endpoint,
			Phase:            v1.SandboxReady,
			StartupLatencyMs: latencyMs,
		}}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: child + tokenSecretSuffix, Namespace: sb.Namespace},
			Data:       map[string][]byte{"token": []byte(token), "endpoint": []byte(endpoint)},
		}
		_ = c.Create(context.Background(), secret)
	})
}

// forkObjects lists the sandboxes whose source is fromSandbox, across all
// namespaces, so a test can assert a rejected fork created NOTHING.
func forkObjects(t *testing.T, c client.Client) []v1.Sandbox {
	t.Helper()
	var list v1.SandboxList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	var forks []v1.Sandbox
	for i := range list.Items {
		if list.Items[i].Spec.Source.FromSandbox != nil {
			forks = append(forks, list.Items[i])
		}
	}
	return forks
}

// TestForkCreatesFromSandboxAndReturnsToken is the happy path for the hosted
// live fork (issue #709): POST /v1/sandboxes/{id}/fork with a Ready org-owned
// source builds a Sandbox whose source is FromSandbox naming the source (NO
// PoolRef: the child boots from the source's live memory, not the cold
// template), polls it to Ready, and returns the create-shaped 201 payload (id,
// endpoint, phase, token, template_id, fork_time_ms) so the flat SDK's
// DirectSandbox constructor needs no change.
func TestForkCreatesFromSandboxAndReturnsToken(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "src-token")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	stop := flipForkToReadyWhenCreated(t, c, orgA, "10.9.9.9:9091", "child-token", 42)
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork",
		Body: []byte(`{"id":"child","template":"python","pause_source":true}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if m["phase"] != "Ready" {
		t.Errorf("phase = %v, want Ready", m["phase"])
	}
	if m["endpoint"] != "10.9.9.9:9091" {
		t.Errorf("endpoint = %v, want the fork child's endpoint", m["endpoint"])
	}
	if m["token"] != "child-token" {
		t.Errorf("token = %v, want the per-child Secret token", m["token"])
	}
	// The child's token, not the source's: a fork must never hand out the
	// source sandbox's credential (the controller's reissue default).
	if m["token"] == "src-token" {
		t.Error("fork response returned the SOURCE token; the reissue boundary is broken")
	}
	// template_id resolves through the source's pool so the SDK's child object
	// carries the same template as its parent.
	if m["template_id"] != "default" {
		t.Errorf("template_id = %v, want default (the source's pool)", m["template_id"])
	}
	// fork_time_ms is the engine-measured child latency, never a hardcoded zero.
	if m["fork_time_ms"] != 42.0 {
		t.Errorf("fork_time_ms = %v, want 42 (the child's StartupLatencyMs)", m["fork_time_ms"])
	}
	name, _ := m["id"].(string)
	if !strings.HasPrefix(name, "sb-") {
		t.Errorf("id = %v, want sb- prefix", m["id"])
	}

	// Verify the object: org namespace, org label, fromSandbox source naming the
	// source, pauseSource carried, and NO poolRef.
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgA), Name: name}, &sb); err != nil {
		t.Fatalf("get created fork: %v", err)
	}
	if sb.Labels[tenant.OrgLabelKey] != orgA {
		t.Errorf("org label = %q, want %q", sb.Labels[tenant.OrgLabelKey], orgA)
	}
	if sb.Spec.Source.FromSandbox == nil || sb.Spec.Source.FromSandbox.Name != "sb-src" {
		t.Errorf("source = %+v, want fromSandbox naming sb-src", sb.Spec.Source)
	}
	if sb.Spec.Source.PoolRef != nil {
		t.Errorf("poolRef = %+v, want nil (a live fork boots from the source, not a pool)", sb.Spec.Source.PoolRef)
	}
	if !sb.Spec.Source.FromSandbox.PauseSource {
		t.Error("pause_source=true was not carried onto spec.source.fromSandbox.pauseSource")
	}
}

// TestForkUnknownSourceIsInstantNotFound asserts a fork naming a source that
// does not exist in the org namespace is an INSTANT LLM-legible 404 naming the
// id (the #646 pool fast-fail precedent), never a created Sandbox that pends
// until the ready timeout.
func TestForkUnknownSourceIsInstantNotFound(t *testing.T) {
	c := newFakeClient(t)
	// A long ready timeout would make a regression (create then poll) hang; the
	// pre-check must return before any poll.
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-ghost/fork", Body: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "sb-ghost") {
		t.Errorf("error does not name the source id: %s", body)
	}
	if !strings.Contains(body, "not_found") {
		t.Errorf("error is not shaped as not_found: %s", body)
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Errorf("created %d fork objects for an unknown source", len(forks))
	}
}

// TestForkSourceNotReadyIsConflict asserts forking a source that exists but is
// not running is a 409 conflict-style typed error with remediation: a live fork
// copies the source VM's running memory, so a Pending source can never serve it.
func TestForkSourceNotReadyIsConflict(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-cold", "", "tok")
	src.Status.Phase = v1.SandboxPending
	src.Status.Endpoint = ""
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-cold/fork", Body: []byte(`{}`),
	})
	if resp.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "Pending") {
		t.Errorf("error does not name the source phase: %s", body)
	}
	if !strings.Contains(body, "remediation") {
		t.Errorf("error carries no remediation: %s", body)
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Errorf("created %d fork objects for a not-Ready source", len(forks))
	}
}

// TestForkCrossOrgSourceIsNotFoundAndNeverForks is the ADVERSARIAL org-scoping
// case: org A's key naming org B's sandbox id in the fork path gets 404,
// indistinguishable from a missing id, and NO fork object is ever created. The
// OrgID comes only from the verified key, so id-guessing across orgs yields
// nothing.
func TestForkCrossOrgSourceIsNotFoundAndNeverForks(t *testing.T) {
	b1, s1 := readySandbox(orgB, "sb-bsource", "2.2.2.2:9091", "tokb")
	c := newFakeClient(t, b1, s1)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	cross, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-bsource/fork", Body: []byte(`{}`),
	})
	if cross.Status != http.StatusNotFound {
		t.Fatalf("cross-org fork: status = %d, want 404", cross.Status)
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Fatalf("cross-org fork CREATED %d fork objects; the boundary failed", len(forks))
	}

	// A truly missing id must be indistinguishable modulo the id (no oracle).
	missing, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-nothere/fork", Body: []byte(`{}`),
	})
	if missing.Status != cross.Status {
		t.Errorf("missing-id status = %d, cross-org status = %d; the two must not differ (oracle)", missing.Status, cross.Status)
	}
	crossBody := strings.ReplaceAll(string(cross.Body), "sb-bsource", "<id>")
	missingBody := strings.ReplaceAll(string(missing.Body), "sb-nothere", "<id>")
	if crossBody != missingBody {
		t.Errorf("cross-org body %q differs from missing-id body %q; a probe can map ids", crossBody, missingBody)
	}
}

// TestForkRejectsReplicasGreaterThanOne asserts a fork body asking for more
// than one child is a typed invalid_input naming the limitation: the gateway
// fork response is create-shaped (ONE id, endpoint, token), so an N-child
// fan-out cannot be represented on this route; the SDK forks n by calling the
// route n times.
func TestForkRejectsReplicasGreaterThanOne(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	for _, body := range []string{`{"replicas":2}`, `{"count":3}`} {
		resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
			OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-src/fork", Body: []byte(body),
		})
		if resp.Status != http.StatusBadRequest {
			t.Errorf("body=%q: status = %d, want 400; body = %s", body, resp.Status, resp.Body)
		}
		if !strings.Contains(string(resp.Body), "invalid_input") {
			t.Errorf("body=%q: error is not typed invalid_input: %s", body, resp.Body)
		}
		if !strings.Contains(string(resp.Body), "one child") {
			t.Errorf("body=%q: error does not name the single-child limitation: %s", body, resp.Body)
		}
	}
	if forks := forkObjects(t, c); len(forks) != 0 {
		t.Errorf("created %d fork objects for a rejected replicas request", len(forks))
	}
}

// TestForkRejectedConditionSurfacesConflict asserts a fork the controller
// rejects terminally (the Rejected condition, for example the
// secret-inheritance default-deny) surfaces the controller's actionable message
// as a 409, never a misleading ready-timeout 504.
func TestForkRejectedConditionSurfacesConflict(t *testing.T) {
	src, srcSecret := readySandbox(orgA, "sb-src", "10.0.0.1:9091", "tok")
	c := newFakeClient(t, src, srcSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
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
	if resp.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "secretInheritance") {
		t.Errorf("rejection message not surfaced: %s", resp.Body)
	}
}

// TestCreateForkTimeMsReportsEngineLatency asserts the create response's
// fork_time_ms is the HONEST engine-measured startup latency the controller
// recorded (status.startupLatencyMs), not a hardcoded 0.0.
func TestCreateForkTimeMsReportsEngineLatency(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "python"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))
	stop := flipWhenCreated(t, c, orgA, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxReady
		sb.Status.Endpoint = "10.1.2.3:9091"
		sb.Status.StartupLatencyMs = 17
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sb.Name + tokenSecretSuffix, Namespace: sb.Namespace},
			Data:       map[string][]byte{"token": []byte("tok"), "endpoint": []byte("10.1.2.3:9091")},
		}
		_ = c.Create(context.Background(), secret)
	})
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	if got := decodeBody(t, resp.Body)["fork_time_ms"]; got != 17.0 {
		t.Errorf("fork_time_ms = %v, want 17 (status.startupLatencyMs, not a hardcoded 0.0)", got)
	}
}

// TestForkedSandboxRuntimeProxyReachesChild asserts a fork returned by the
// gateway is NOT a dead end: a runtime call addressed to the fork's id proxies
// to the fork CHILD's endpoint with the per-child token (the fork object itself
// never carries Status.Endpoint; its children do).
func TestForkedSandboxRuntimeProxyReachesChild(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/connect+json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	ns := tenant.NamespaceForOrg(orgA)
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-fk", Namespace: ns, Labels: tenant.OrgLabels(orgA)},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{
			FromSandbox: &v1.FromSandboxSource{Name: "sb-src"},
		}},
		Status: v1.SandboxStatus{
			Phase:         v1.SandboxReady,
			ReadyReplicas: 1,
			Children: []v1.SandboxChild{{
				Name: "sb-fk-fork-0", SandboxID: "sb-fk-fork-0", Endpoint: endpoint, Phase: v1.SandboxReady,
			}},
		},
	}
	childSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-fk-fork-0" + tokenSecretSuffix, Namespace: ns},
		Data:       map[string][]byte{"token": []byte("child-tok"), "endpoint": []byte(endpoint)},
	}
	c := newFakeClient(t, fork, childSecret)
	cp := New(c, WithHTTPClient(srv.Client()))

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/v1/sandboxes/sb-fk/exec", Method: http.MethodPost,
		BodyStream: strings.NewReader(`{"cmd":["true"]}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusOK {
		var body []byte
		if resp.BodyStream != nil {
			body, _ = io.ReadAll(resp.BodyStream)
			_ = resp.BodyStream.Close()
		} else {
			body = resp.Body
		}
		t.Fatalf("status = %d, body = %s (a fork id must proxy to its child endpoint)", resp.Status, body)
	}
	if resp.BodyStream != nil {
		_, _ = io.Copy(io.Discard, resp.BodyStream)
		_ = resp.BodyStream.Close()
	}
	if gotAuth != "Bearer child-tok" {
		t.Errorf("upstream Authorization = %q, want the per-CHILD token", gotAuth)
	}

	// The status verb reports the child's endpoint too, so a fork is inspectable.
	st, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.status", Path: "/v1/sandboxes/sb-fk",
	})
	var m map[string]any
	if err := json.Unmarshal(st.Body, &m); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if m["endpoint"] != endpoint {
		t.Errorf("status endpoint = %v, want the child endpoint %q", m["endpoint"], endpoint)
	}
}

// forkWithChildren builds a Ready org-A fork object named name whose children
// carry the given phases (child i is "<name>-fork-<i>" at endpoint), plus the
// first child's token Secret. It models the post-create shape the controller's
// fork engine leaves behind.
func forkWithChildren(name, endpoint string, phases ...v1.SandboxPhase) (*v1.Sandbox, *corev1.Secret) {
	ns := tenant.NamespaceForOrg(orgA)
	children := make([]v1.SandboxChild, 0, len(phases))
	for i, ph := range phases {
		child := fmt.Sprintf("%s-fork-%d", name, i)
		children = append(children, v1.SandboxChild{
			Name: child, SandboxID: child, Endpoint: endpoint, Phase: ph,
		})
	}
	fork := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: tenant.OrgLabels(orgA)},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{
			FromSandbox: &v1.FromSandboxSource{Name: "sb-src"},
		}},
		Status: v1.SandboxStatus{
			Phase:         v1.SandboxReady,
			ReadyReplicas: int32(len(phases)),
			Children:      children,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-fork-0" + tokenSecretSuffix, Namespace: ns},
		Data:       map[string][]byte{"token": []byte("child-tok"), "endpoint": []byte(endpoint)},
	}
	return fork, secret
}

// TestForkChildTerminatedYieldsTypedErrorNotProxied asserts a fork whose ONLY
// child was reaped (the GC flips the CHILD phase while the parent fork object
// stays Ready) answers a runtime call with the documented typed idle_timeout
// error BEFORE any dial, exactly like a reaped pool claim (issue #688), never a
// generic 502 against the dead child endpoint.
func TestForkChildTerminatedYieldsTypedErrorNotProxied(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	fork, childSecret := forkWithChildren("sb-fk", endpoint, v1.SandboxTerminated)
	c := newFakeClient(t, fork, childSecret)
	cp := New(c, WithHTTPClient(srv.Client()))

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/v1/sandboxes/sb-fk/exec", Method: http.MethodPost,
		BodyStream: strings.NewReader(`{}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if reached {
		t.Fatal("runtime call for a reaped fork child REACHED the dead endpoint; it must fail typed before any dial")
	}
	if resp.Status != http.StatusGone {
		t.Fatalf("status = %d, want 410 (typed idle_timeout); body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "idle_timeout") {
		t.Errorf("error is not the typed idle_timeout: %s", resp.Body)
	}

	// The WebSocket resolver consults the same gate.
	if _, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-fk"); aerr == nil || aerr.Code != "idle_timeout" {
		t.Errorf("ResolveRuntime error = %+v, want typed idle_timeout", aerr)
	}
}

// TestForkMultiChildRuntimeIsTypedErrorNotChildZero asserts the gateway runtime
// surface refuses a fork fan-out with MORE than one child (created by another
// client, for example the cluster SDK) with a typed error naming the
// single-child limitation, instead of silently routing every call to child 0
// while children 1..N-1 are unreachable.
func TestForkMultiChildRuntimeIsTypedErrorNotChildZero(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint := strings.TrimPrefix(srv.URL, "http://")

	fork, childSecret := forkWithChildren("sb-fan", endpoint, v1.SandboxReady, v1.SandboxReady)
	c := newFakeClient(t, fork, childSecret)
	cp := New(c, WithHTTPClient(srv.Client()))

	// Runtime proxy (exec/files/run_code).
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/v1/sandboxes/sb-fan/exec", Method: http.MethodPost,
		BodyStream: strings.NewReader(`{}`),
	})
	if reached {
		t.Fatal("multi-child fork runtime call was silently routed to child 0")
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("proxy status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "single-child") {
		t.Errorf("proxy error does not name the single-child limitation: %s", resp.Body)
	}

	// The WebSocket resolver.
	if _, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-fan"); aerr == nil || !strings.Contains(aerr.Cause, "single-child") {
		t.Errorf("ResolveRuntime error = %+v, want the single-child limitation", aerr)
	}

	// Pause/resume lifecycle.
	lresp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.pause", Path: "/v1/pause", Body: []byte(`{"sandbox":"sb-fan"}`),
	})
	if reached {
		t.Fatal("multi-child fork pause was silently routed to child 0")
	}
	if lresp.Status != http.StatusBadRequest || !strings.Contains(string(lresp.Body), "single-child") {
		t.Errorf("pause status = %d body = %s, want 400 naming the single-child limitation", lresp.Status, lresp.Body)
	}
}

// TestForkOfForkIsRejected asserts forking a source that is itself a
// fromSandbox fork is a typed invalid_input naming the limitation: the running
// VM is the fork's CHILD, not the fork object the new FromSandbox would name,
// and the controller fork-of-fork resolution is not proven on this surface, so
// the honest answer is a reject with remediation, not an unverified fork.
func TestForkOfForkIsRejected(t *testing.T) {
	fork, childSecret := forkWithChildren("sb-parentfork", "10.9.9.9:9091", v1.SandboxReady)
	c := newFakeClient(t, fork, childSecret)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.fork", Path: "/v1/sandboxes/sb-parentfork/fork", Body: []byte(`{}`),
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	body := string(resp.Body)
	if !strings.Contains(body, "invalid_input") {
		t.Errorf("error is not typed invalid_input: %s", body)
	}
	if !strings.Contains(body, "itself a live fork") {
		t.Errorf("error does not name the fork-of-fork limitation: %s", body)
	}
	if !strings.Contains(body, "remediation") {
		t.Errorf("error carries no remediation: %s", body)
	}
	// Only the pre-seeded parent fork exists; no new fork object was created.
	if forks := forkObjects(t, c); len(forks) != 1 {
		t.Errorf("fork objects = %d, want only the pre-seeded parent (nothing created)", len(forks))
	}
}

// TestForkMultiChildWithTerminatedFirstChildStillSingleChildError asserts the
// multi-child guard OUTRANKS the child-terminal gate: a multi-child fan-out
// whose FIRST child happens to be reaped must still get the documented
// single-child limitation error, not child 0's idle_timeout, because this
// surface refuses to interpret any one child of a fan-out (CodeRabbit review
// of #710).
func TestForkMultiChildWithTerminatedFirstChildStillSingleChildError(t *testing.T) {
	fork, childSecret := forkWithChildren("sb-fanx", "10.9.9.9:9091", v1.SandboxTerminated, v1.SandboxReady)
	c := newFakeClient(t, fork, childSecret)
	cp := New(c)

	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.runtime", Path: "/v1/sandboxes/sb-fanx/exec", Method: http.MethodPost,
		BodyStream: strings.NewReader(`{}`),
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("proxy status = %d, want 400 (single-child limitation); body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "single-child") {
		t.Errorf("proxy error does not name the single-child limitation: %s", resp.Body)
	}
	if strings.Contains(string(resp.Body), "idle_timeout") {
		t.Errorf("multi-child fork wrongly answered with child 0's idle_timeout: %s", resp.Body)
	}

	if _, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-fanx"); aerr == nil || !strings.Contains(aerr.Cause, "single-child") {
		t.Errorf("ResolveRuntime error = %+v, want the single-child limitation", aerr)
	}
}
