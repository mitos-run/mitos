package controlplane

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
)

// poolGetCountingClient wraps the fake client and counts Gets of SandboxPool
// objects, the typo fast-fail pre-check round trip on the create hot path.
func poolGetCountingClient(t *testing.T, gets *atomic.Int64, objs ...client.Object) client.Client {
	t.Helper()
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(objs...).
		Build()
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1.SandboxPool); ok {
				gets.Add(1)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
}

// createOnce drives one create through Forward as org, flipping the sandbox
// Ready in the background, and returns the response.
func createOnce(t *testing.T, cp *K8sControlPlane, c client.Client, org, pool string) saas.ForwardResponse {
	t.Helper()
	stop := flipToReadyWhenCreated(t, c, org, "10.1.2.3:9091", "tok-pool")
	defer stop()
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: org, Op: "sandbox.create", Body: []byte(`{"pool":"` + pool + `"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	return resp
}

// TestRepeatCreateSkipsThePoolPreCheckRoundTrip asserts the typo fast-fail
// pool pre-check is paid at most once per TTL window: hosted pools are
// pre-provisioned and stable, so a positive existence result is cached and a
// repeat create of the same pool performs no SandboxPool Get. The pre-check is
// a UX guard, not a correctness gate; a pool deleted inside the window falls
// through to the controller's bounded grace (#637), the same path a direct
// create takes.
func TestRepeatCreateSkipsThePoolPreCheckRoundTrip(t *testing.T) {
	var poolGets atomic.Int64
	c := poolGetCountingClient(t, &poolGets, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	if resp := createOnce(t, cp, c, orgA, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", resp.Status, resp.Body)
	}
	if resp := createOnce(t, cp, c, orgA, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("second create status = %d, body = %s", resp.Status, resp.Body)
	}
	if n := poolGets.Load(); n != 1 {
		t.Fatalf("two creates of one stable pool performed %d SandboxPool Get(s), want 1: the positive pre-check must be cached", n)
	}
}

// TestPoolPreCheckCacheIsNamespaceScoped asserts a positive cache entry for
// one org's namespace never satisfies the pre-check for the SAME pool name in
// another org's namespace: the cache key carries the namespace, so each tenant
// pays (and proves) its own existence check.
func TestPoolPreCheckCacheIsNamespaceScoped(t *testing.T) {
	var poolGets atomic.Int64
	c := poolGetCountingClient(t, &poolGets, poolIn(orgA, "default"), poolIn(orgB, "default"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	if resp := createOnce(t, cp, c, orgA, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("org A create status = %d, body = %s", resp.Status, resp.Body)
	}
	if resp := createOnce(t, cp, c, orgB, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("org B create status = %d, body = %s", resp.Status, resp.Body)
	}
	if n := poolGets.Load(); n != 2 {
		t.Fatalf("two orgs creating the same pool name performed %d SandboxPool Get(s), want 2: a cache hit must never cross namespaces", n)
	}
}

// TestUnknownPoolIsReCheckedEveryCreate asserts absence is NEVER cached: every
// create naming a missing pool re-reads authoritatively and 404s instantly, so
// a pool that appears between two attempts is seen immediately.
func TestUnknownPoolIsReCheckedEveryCreate(t *testing.T) {
	var poolGets atomic.Int64
	c := poolGetCountingClient(t, &poolGets)
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	for i := 0; i < 2; i++ {
		resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
			OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"nope"}`),
		})
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if resp.Status != http.StatusNotFound {
			t.Fatalf("create %d status = %d, want 404; body = %s", i, resp.Status, resp.Body)
		}
		if !strings.Contains(string(resp.Body), "no such pool") || !strings.Contains(string(resp.Body), "nope") {
			t.Errorf("create %d body missing the pool name: %s", i, resp.Body)
		}
	}
	if n := poolGets.Load(); n != 2 {
		t.Fatalf("two creates of a missing pool performed %d SandboxPool Get(s), want 2: absence must never be cached", n)
	}
}

// TestPoolPreCheckCacheExpires asserts the positive cache is bounded by its
// TTL: once the window passes, the next create re-reads authoritatively.
func TestPoolPreCheckCacheExpires(t *testing.T) {
	var poolGets atomic.Int64
	c := poolGetCountingClient(t, &poolGets, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	if resp := createOnce(t, cp, c, orgA, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", resp.Status, resp.Body)
	}

	// Step the control plane's clock past the TTL; timers derived from it stay
	// sane because the offset is constant.
	cp.now = func() time.Time { return time.Now().Add(poolCheckTTL + time.Second) }

	if resp := createOnce(t, cp, c, orgA, "default"); resp.Status != http.StatusCreated {
		t.Fatalf("second create status = %d, body = %s", resp.Status, resp.Body)
	}
	if n := poolGets.Load(); n != 2 {
		t.Fatalf("a create after the TTL performed %d total SandboxPool Get(s), want 2: the cache must expire", n)
	}
}
