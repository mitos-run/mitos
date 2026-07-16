package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// TestWatchDrivenCreateNeedsNoSandboxGet asserts the watch-driven create hot
// path performs ZERO Gets on the Sandbox object: the ready watch is established
// BEFORE the Create, so the create event itself arrives on the watch and no
// authoritative re-read is needed to close a missed-event window. Every
// serialized apiserver round trip here is paid by every hosted create (prod
// measured the control-plane share of create at ~140 ms P50 against a 60-76 ms
// engine restore), so the round-trip count is the property under test, the same
// way the SDK pins its connection count.
func TestWatchDrivenCreateNeedsNoSandboxGet(t *testing.T) {
	var sandboxGets atomic.Int64
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(poolIn(orgA, "default")).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1.Sandbox); ok {
				sandboxGets.Add(1)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(10*time.Second))

	stop := flipToReadyWhenCreated(t, c, orgA, "10.1.2.3:9091", "tok-noget")
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
	if n := sandboxGets.Load(); n != 0 {
		t.Fatalf("watch-driven create performed %d Sandbox Get(s); the watch must be established before the Create so no authoritative re-read is needed", n)
	}
}

// TestReadyFlipDuringCreateWriteIsStillSeen asserts a phase flip that lands
// BEFORE the Create call even returns (the fastest controller imaginable) is
// still observed. This is the race the post-establish authoritative Get used
// to close; with the watch established before the Create, the events
// themselves cover it, and this test keeps that property pinned.
func TestReadyFlipDuringCreateWriteIsStillSeen(t *testing.T) {
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(poolIn(orgA, "default")).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			sb, ok := obj.(*v1.Sandbox)
			if !ok {
				return nil
			}
			ready := sb.DeepCopy()
			ready.Status.Phase = v1.SandboxReady
			ready.Status.Endpoint = "10.9.9.9:9091"
			if err := cl.Status().Update(ctx, ready); err != nil {
				return err
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: sb.Name + tokenSecretSuffix, Namespace: sb.Namespace},
				Data:       map[string][]byte{"token": []byte("tok-flip"), "endpoint": []byte("10.9.9.9:9091")},
			}
			return cl.Create(ctx, secret)
		},
	})
	cp := New(c, WithPollInterval(3*time.Second), WithReadyTimeout(10*time.Second))

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
		t.Fatalf("Ready flip during the Create write surfaced after %s; it must be seen immediately, not on a tick or fallback boundary", elapsed)
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
