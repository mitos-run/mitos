package controller_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// routeRecorder is a concurrency-safe httptest handler that records the last
// route set posted to /internal/routes.
type routeRecorder struct {
	mu   sync.Mutex
	last []controller.ExposeRoute
}

func (r *routeRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/internal/routes" || req.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload struct {
		Routes []controller.ExposeRoute `json:"routes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.last = payload.Routes
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *routeRecorder) snapshot() []controller.ExposeRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]controller.ExposeRoute(nil), r.last...)
}

// TestExposeReconcilerSyncsReadyRoutes is the envtest-based integration test
// for ExposeRouteReconciler. It uses direct Reconcile calls (no manager
// registration) to avoid coupling to the shared suite manager.
func TestExposeReconcilerSyncsReadyRoutes(t *testing.T) {
	rec := &routeRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	r := &controller.ExposeRouteReconciler{
		Client: k8sClient,
		Scheme: scheme,
		Poster: controller.NewExposePoster(srv.URL, "admintok"),
	}

	// Use a unique namespace prefix to avoid cross-test interference: the
	// envtest API server is shared and not reset between tests.
	ns := fmt.Sprintf("expose-test-%d", time.Now().UnixNano())
	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}
	if err := k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, nsObj) })

	sbName := "sb-expose-ready"
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sbName, Namespace: ns},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: "irrelevant"},
			},
			Expose: &v1.SandboxExpose{
				Port:    8080,
				Label:   "openclaw",
				Sharing: "private",
			},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sb) })

	// Status is a subresource; set it via Status().Update.
	sb.Status.Phase = v1.SandboxReady
	sb.Status.Endpoint = "10.0.0.7:9091"
	sb.Status.SandboxID = "sbx-abc"
	if err := k8sClient.Status().Update(ctx, sb); err != nil {
		t.Fatalf("update sandbox status: %v", err)
	}

	// Create the per-sandbox token Secret.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbName + "-sandbox-token",
			Namespace: ns,
		},
		Data: map[string][]byte{
			"token": []byte("per-sb-tok"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("create token secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	// Drive Reconcile directly.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sbName, Namespace: ns}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	// No requeue expected when all is healthy.
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", result.RequeueAfter)
	}

	routes := rec.snapshot()
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d: %+v", len(routes), routes)
	}
	got := routes[0]
	want := controller.ExposeRoute{
		Label:        "openclaw",
		SandboxID:    "sbx-abc",
		NodeEndpoint: "10.0.0.7:9091",
		Port:         8080,
		Token:        "per-sb-tok",
		Sharing:      "private",
		Ready:        true,
	}
	if got != want {
		t.Errorf("route mismatch:\n  got  %+v\n  want %+v", got, want)
	}

	// Now mark the sandbox not-Ready and reconcile again: the route must disappear.
	var fresh v1.Sandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sbName, Namespace: ns}, &fresh); err != nil {
		t.Fatalf("re-get sandbox: %v", err)
	}
	fresh.Status.Phase = v1.SandboxPending
	if err := k8sClient.Status().Update(ctx, &fresh); err != nil {
		t.Fatalf("update sandbox status to Pending: %v", err)
	}

	_, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}

	routes2 := rec.snapshot()
	for _, ro := range routes2 {
		if ro.Label == "openclaw" {
			t.Errorf("route for label 'openclaw' still present after sandbox became not-Ready: %+v", routes2)
		}
	}
}

// TestExposeReconcilerRequeueMissingSecret verifies that a Ready sandbox whose
// token Secret is absent causes a requeue (RequeueAfter) instead of an error,
// and does not include that sandbox in the posted route set.
func TestExposeReconcilerRequeueMissingSecret(t *testing.T) {
	rec := &routeRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	r := &controller.ExposeRouteReconciler{
		Client: k8sClient,
		Scheme: scheme,
		Poster: controller.NewExposePoster(srv.URL, "admintok"),
	}

	ns := fmt.Sprintf("expose-missing-%d", time.Now().UnixNano())
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, nsObj) })

	sbName := "sb-missing-tok"
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sbName, Namespace: ns},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: "irrelevant"},
			},
			Expose: &v1.SandboxExpose{Port: 9000, Label: "missing", Sharing: "link"},
		},
	}
	if err := k8sClient.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, sb) })

	sb.Status.Phase = v1.SandboxReady
	sb.Status.Endpoint = "10.0.0.8:9091"
	sb.Status.SandboxID = "sbx-missing"
	if err := k8sClient.Status().Update(ctx, sb); err != nil {
		t.Fatalf("update sandbox status: %v", err)
	}

	// Intentionally do NOT create the token Secret.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: sbName, Namespace: ns}}
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile with missing secret returned error (want requeue): %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want > 0 when token Secret is missing")
	}

	// The sandbox must not appear in the posted route set (tokenFor returned ok=false).
	routes := rec.snapshot()
	for _, ro := range routes {
		if ro.Label == "missing" {
			t.Errorf("sandbox with missing Secret was included in routes: %+v", routes)
		}
	}
}
