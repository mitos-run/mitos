package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// noWatchClient hides the fake client's Watch method so the control plane sees
// a plain client.Client, forcing the legacy polling path.
type noWatchClient struct {
	client.Client
}

// TestCreateReadyIsWatchDrivenNotTickQuantized asserts the create readiness
// wait is event driven: with a poll interval far larger than the controller's
// flip latency, the create must still return the moment the sandbox goes
// Ready. Under the legacy ticker the response could only arrive on a tick
// boundary (here 3s), which production measured as the dominant create/fork
// cost (p50 545ms client observed vs 6-40ms on-node activate).
func TestCreateReadyIsWatchDrivenNotTickQuantized(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(10*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-watch")
	defer stop()

	started := time.Now()
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	if elapsed >= 1*time.Second {
		t.Fatalf("create took %s; a watch-driven wait must return well before the 3s poll tick", elapsed)
	}
}

// TestWatchSeesFailedWithoutPollTick asserts a Failed phase is surfaced by the
// watch immediately, with the same 502 envelope and rejection message the poll
// path produced.
func TestWatchSeesFailedWithoutPollTick(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(10*time.Second))
	stop := flipWhenCreated(t, c, orgA, func(sb *v1.Sandbox) {
		sb.Status.Phase = v1.SandboxFailed
		sb.Status.Conditions = readyFalseCondition("PoolMissing", "pool default was not found")
	})
	defer stop()

	started := time.Now()
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	elapsed := time.Since(started)
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "pool default was not found") {
		t.Errorf("error body missing the rejection message: %s", resp.Body)
	}
	if elapsed >= 1*time.Second {
		t.Fatalf("Failed surfaced after %s; the watch must deliver it before the 3s poll tick", elapsed)
	}
}

// TestWatchDeletedMidCreateIsNotFound asserts a sandbox deleted while the
// create waits (a terminate racing the create) returns the same not_found
// envelope the poll path produced, driven by the watch Deleted event.
func TestWatchDeletedMidCreateIsNotFound(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(10*time.Second))

	// Delete the sandbox as soon as it appears, mimicking a racing terminate.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ns := tenant.NamespaceForOrg(orgA)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var list v1.SandboxList
			if err := c.List(context.Background(), &list, client.InNamespace(ns)); err == nil && len(list.Items) > 0 {
				_ = c.Delete(context.Background(), &list.Items[0])
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	started := time.Now()
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	<-done
	elapsed := time.Since(started)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "removed before it became ready") {
		t.Errorf("error body missing the removed-mid-create cause: %s", resp.Body)
	}
	if elapsed >= 1*time.Second {
		t.Fatalf("deletion surfaced after %s; the watch must deliver it before the 3s poll tick", elapsed)
	}
}

// TestWatchEstablishFailureFallsBackToPolling asserts the create fails OPEN to
// the legacy poll loop when the watch cannot be established: readiness is
// still observed and the create still succeeds.
func TestWatchEstablishFailureFallsBackToPolling(t *testing.T) {
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(poolIn(orgA, "default")).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Watch: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
			return nil, errors.New("watch is not permitted")
		},
	})
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-fallback")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s (watch failure must fall back to polling, not fail the create)", resp.Status, resp.Body)
	}
}

// TestNoWatchClientFallsBackToPolling asserts a control plane built over a
// client WITHOUT Watch support (older wiring, tests) still serves creates via
// the legacy poll loop.
func TestNoWatchClientFallsBackToPolling(t *testing.T) {
	c := noWatchClient{Client: newFakeClient(t, poolIn(orgA, "default"))}
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-plain")
	defer stop()

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
}

// TestWatchTimeoutKeepsEnvelope asserts a sandbox that never becomes ready
// still times out with the 504 envelope and the same actionable message under
// the watch path.
func TestWatchTimeoutKeepsEnvelope(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(60*time.Millisecond))
	resp, _ := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"default"}`),
	})
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "did not become ready") {
		t.Errorf("timeout error not actionable: %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), "Pending") {
		t.Errorf("timeout error should name the last observed phase: %s", resp.Body)
	}
}

// TestDefaultPollIntervalIs25ms asserts the residual fallback poll is not
// quantized to the old 250ms tick.
func TestDefaultPollIntervalIs25ms(t *testing.T) {
	cp := New(newFakeClient(t))
	if cp.pollInterval != 25*time.Millisecond {
		t.Fatalf("default poll interval = %s, want 25ms", cp.pollInterval)
	}
}

// readyFalseCondition builds the single Ready=False condition the controller
// stamps on a failed claim.
func readyFalseCondition(reason, message string) []metav1.Condition {
	return []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason,
		Message: message, LastTransitionTime: metav1.Now(),
	}}
}
