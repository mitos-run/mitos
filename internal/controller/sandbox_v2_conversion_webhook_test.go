package controller_test

// Conversion webhook envtest (issue #23, ADR 0007): proves the SandboxPool
// conversion webhook is wired into a manager and SERVES both versions through
// the API server. It starts a SEPARATE envtest with a webhook server (the main
// suite runs without one, so it gets strategy None); envtest auto-detects the
// convertible SandboxPool and patches the CRD to route conversions at the test
// webhook. A v1alpha1 pool created through the API is then read back as
// v1alpha2 (and vice versa), exercising ConvertFrom and ConvertTo end to end.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	v1alpha2 "mitos.run/mitos/api/v1alpha2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func TestSandboxPoolConversionWebhookServesBothVersions(t *testing.T) {
	whScheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(whScheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(whScheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha2.AddToScheme(whScheme); err != nil {
		t.Fatal(err)
	}

	env := &envtest.Environment{
		Scheme: whScheme,
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "crds"),
		},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest with webhook: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	wctx, wcancel := context.WithCancel(context.Background())
	t.Cleanup(wcancel)

	whOpts := env.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  whScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    whOpts.LocalServingHost,
			Port:    whOpts.LocalServingPort,
			CertDir: whOpts.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if err := v1alpha2.SetupSandboxPoolWebhookWithManager(mgr); err != nil {
		t.Fatalf("setup conversion webhook: %v", err)
	}
	go func() { _ = mgr.Start(wctx) }()

	c, err := client.New(cfg, client.Options{Scheme: whScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// Wait for the webhook server to be serving (the conversion path is only ready
	// once the manager's webhook server is up).
	waitWebhookReady(t, &env.WebhookInstallOptions)

	// Create a v1alpha1 (Hub / storage version) pool with a templateRef and an
	// autoscale block, then read it back AS v1alpha2 through the API. The webhook
	// runs ConvertFrom; the warm/snapshots regrouping must be present.
	hub := &v1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "wh-pool", Namespace: "default"},
		Spec: v1alpha1.SandboxPoolSpec{
			TemplateRef: v1alpha1.LocalObjectReference{Name: "wh-tmpl"},
			Replicas:    4,
			Autoscale: &v1alpha1.PoolAutoscaleSpec{
				MinWarm: 2, MaxWarm: 20, TargetSpare: 3, ScaleDownCooldownSeconds: 90,
			},
		},
	}
	if err := c.Create(context.Background(), hub); err != nil {
		t.Fatalf("create v1alpha1 pool: %v", err)
	}

	var asV2 v1alpha2.SandboxPool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wh-pool", Namespace: "default"}, &asV2); err != nil {
		t.Fatalf("get pool as v1alpha2 (conversion webhook): %v", err)
	}
	if asV2.Spec.TemplateRef == nil || asV2.Spec.TemplateRef.Name != "wh-tmpl" {
		t.Fatalf("v2 templateRef not converted: %+v", asV2.Spec.TemplateRef)
	}
	if asV2.Spec.Warm == nil || asV2.Spec.Warm.Min != 2 || asV2.Spec.Warm.Max != 20 ||
		asV2.Spec.Warm.TargetPending != 3 || asV2.Spec.Warm.CooldownSeconds != 90 {
		t.Fatalf("v2 warm not converted from autoscale: %+v", asV2.Spec.Warm)
	}
	if asV2.Spec.Snapshots == nil || asV2.Spec.Snapshots.ReplicasPerNode != 4 {
		t.Fatalf("v2 snapshots not converted: %+v", asV2.Spec.Snapshots)
	}

	// Read the SAME object as v1alpha1 again: ConvertTo round-trips the value.
	var backV1 v1alpha1.SandboxPool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wh-pool", Namespace: "default"}, &backV1); err != nil {
		t.Fatalf("get pool as v1alpha1: %v", err)
	}
	if backV1.Spec.Replicas != 4 || backV1.Spec.Autoscale == nil || backV1.Spec.Autoscale.MaxWarm != 20 {
		t.Fatalf("v1 round-trip lost values: %+v", backV1.Spec)
	}
}

// waitWebhookReady polls the webhook server's serving port until it accepts a
// TCP connection, so the conversion call does not race the server start.
func waitWebhookReady(t *testing.T, opts *envtest.WebhookInstallOptions) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	addr := net.JoinHostPort(opts.LocalServingHost, fmt.Sprintf("%d", opts.LocalServingPort))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("webhook server at %s did not start within 20s", addr)
}
