package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
)

// TestReconcilePoolRefFailsFastOnMissingPool proves that a poolRef Sandbox naming
// a pool that does not exist fails fast to a terminal Failed phase with an
// actionable PoolNotFound condition, in a SINGLE reconcile, instead of requeuing
// forever and leaving the create to hang the full ready-timeout. A genuinely
// missing pool is a caller typo, not a transient error (issue #28 LLM-legible
// errors; the no-dead-ends journey rule).
func TestReconcilePoolRefFailsFastOnMissingPool(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-typo", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: "does-not-exist"},
			},
		},
		Status: v1.SandboxStatus{Phase: v1.SandboxPending},
	}

	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(claim).
		Build()
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	res, err := r.reconcilePoolRef(ctx, claim)
	if err != nil {
		t.Fatalf("reconcilePoolRef returned an error (should fail the claim terminally, not requeue): %v", err)
	}
	// A NotFound pool is terminal: no requeue. A requeue here would re-hammer the
	// missing pool forever and hang the create for the full ready-timeout.
	if res.RequeueAfter != 0 {
		t.Fatalf("reconcilePoolRef requeued on a missing pool (RequeueAfter=%v); want terminal no-requeue", res.RequeueAfter)
	}

	var got v1.Sandbox
	if err := c.Get(ctx, client.ObjectKeyFromObject(claim), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1.SandboxFailed {
		t.Fatalf("phase = %q, want %q (the claim must not stay Pending / requeue forever)", got.Status.Phase, v1.SandboxFailed)
	}
	// FinishedAt must be stamped so the GC TTL pass reaps this terminal claim;
	// without it a typo'd-pool Failed sandbox leaks in etcd forever (matching the
	// other terminal-Failed paths in reconcilePoolRef).
	if got.Status.FinishedAt == nil {
		t.Errorf("FinishedAt not stamped on the terminal Failed claim; the GC TTL pass would never reap it")
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatalf("no Ready condition set on the failed claim")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition status = %q, want False", cond.Status)
	}
	if cond.Reason != "PoolNotFound" {
		t.Errorf("Ready condition reason = %q, want PoolNotFound", cond.Reason)
	}
	if !strings.Contains(cond.Message, "does-not-exist") || !strings.Contains(cond.Message, "default") {
		t.Errorf("condition message must name the pool and namespace, got: %q", cond.Message)
	}
}

// TestReconcilePoolRefRequeuesOnCacheLag proves that a pool the CACHED client has
// not seen yet (a just-created pool, informer-cache lag) is NOT mistaken for a
// caller typo: the reconciler confirms against the uncached apiserver
// (r.APIReader), finds the pool there, and requeues instead of failing the claim.
// Without this, create-pool-then-create-sandbox would wrongly reject a valid
// sandbox on the racy first reconcile (this is what broke TestClaimMaxLifetimeReaped).
func TestReconcilePoolRefRequeuesOnCacheLag(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-lag", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "lagging-pool"}},
		},
		Status: v1.SandboxStatus{Phase: v1.SandboxPending},
	}
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lagging-pool", Namespace: "default"},
	}

	// Cached client: has the claim but NOT the pool (informer has not caught up).
	cached := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(claim).
		Build()
	// Uncached apiserver reader: the pool IS present (it was created).
	uncached := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool).
		Build()
	r := &SandboxReconciler{Client: cached, APIReader: uncached}
	ctx := context.Background()

	res, err := r.reconcilePoolRef(ctx, claim)
	if err != nil {
		t.Fatalf("reconcilePoolRef errored on a cache-lag pool: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a requeue to let the cache catch up, got RequeueAfter=%v", res.RequeueAfter)
	}
	var got v1.Sandbox
	if err := cached.Get(ctx, client.ObjectKeyFromObject(claim), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("a cache-lag pool wrongly failed a valid sandbox with PoolNotFound")
	}
}

// TestReconcilePoolRefRequeuesOnTransientPoolGetError proves a NON-NotFound
// (transient apiserver) error on the pool Get keeps the current requeue behavior:
// a flaky apiserver must NOT wrongly fail a valid sandbox. The reconcile returns
// the error (controller-runtime requeues) and the claim is not marked Failed.
func TestReconcilePoolRefRequeuesOnTransientPoolGetError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-flaky", Namespace: "default"},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: "real-pool"},
			},
		},
		Status: v1.SandboxStatus{Phase: v1.SandboxPending},
	}

	base := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(claim).
		Build()
	// Wrap the pool Get with a transient (non-NotFound) server error.
	c := &transientPoolGetClient{Client: base}
	r := &SandboxReconciler{Client: c}
	ctx := context.Background()

	_, err := r.reconcilePoolRef(ctx, claim)
	if err == nil {
		t.Fatalf("reconcilePoolRef swallowed a transient pool Get error; a flaky apiserver must requeue, not fail the claim")
	}

	var got v1.Sandbox
	if err := base.Get(ctx, client.ObjectKeyFromObject(claim), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == v1.SandboxFailed {
		t.Fatalf("a transient pool Get error wrongly failed a valid sandbox")
	}
}

// transientPoolGetClient returns a synthetic server error on a SandboxPool Get,
// modeling a flaky apiserver (not a genuine NotFound).
type transientPoolGetClient struct {
	client.Client
}

func (t *transientPoolGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1.SandboxPool); ok {
		return apierrors.NewInternalError(errTransientPoolGet)
	}
	return t.Client.Get(ctx, key, obj, opts...)
}

var errTransientPoolGet = &transientErr{}

type transientErr struct{}

func (*transientErr) Error() string { return "synthetic transient apiserver error" }
