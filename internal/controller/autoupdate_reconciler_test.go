package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/runmanifest"
)

type fakeRegistry struct {
	ref string
	err error
}

func (f fakeRegistry) Latest(context.Context, string, string) (string, error) {
	return f.ref, f.err
}

func autoUpdateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func trackedPool(resolved string) *v1.SandboxPool {
	return &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openclaw",
			Namespace: "ns",
			Annotations: map[string]string{
				runmanifest.AnnTrackWatch:    "ghcr.io/openclaw/openclaw",
				runmanifest.AnnTrackChannel:  "latest",
				runmanifest.AnnResolvedImage: resolved,
			},
		},
		Spec: v1.SandboxPoolSpec{Template: &v1.PoolTemplateSpec{Image: resolved}},
	}
}

func reconcileOpenclaw(t *testing.T, c client.Client, reg RegistryChecker) {
	t.Helper()
	r := &AutoUpdateReconciler{Client: c, Registry: reg}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "openclaw"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestAutoUpdateResnapshotsOnNewRelease(t *testing.T) {
	pool := trackedPool("ghcr.io/openclaw/openclaw@sha256:old")
	c := fake.NewClientBuilder().WithScheme(autoUpdateScheme(t)).WithObjects(pool).Build()
	reconcileOpenclaw(t, c, fakeRegistry{ref: "ghcr.io/openclaw/openclaw@sha256:new"})

	var got v1.SandboxPool
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "openclaw"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Template.Image != "ghcr.io/openclaw/openclaw@sha256:new" {
		t.Errorf("golden image not re-snapshotted: %q", got.Spec.Template.Image)
	}
	if got.Annotations[runmanifest.AnnResolvedImage] != "ghcr.io/openclaw/openclaw@sha256:new" {
		t.Errorf("resolved-image annotation not advanced: %q", got.Annotations[runmanifest.AnnResolvedImage])
	}
}

func TestAutoUpdateNoChange(t *testing.T) {
	pool := trackedPool("ghcr.io/openclaw/openclaw@sha256:same")
	c := fake.NewClientBuilder().WithScheme(autoUpdateScheme(t)).WithObjects(pool).Build()
	reconcileOpenclaw(t, c, fakeRegistry{ref: "ghcr.io/openclaw/openclaw@sha256:same"})

	var got v1.SandboxPool
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "openclaw"}, &got)
	if got.Spec.Template.Image != "ghcr.io/openclaw/openclaw@sha256:same" {
		t.Errorf("image should be unchanged, got %q", got.Spec.Template.Image)
	}
}

func TestAutoUpdateIgnoresUntrackedPool(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "openclaw", Namespace: "ns"},
		Spec:       v1.SandboxPoolSpec{Template: &v1.PoolTemplateSpec{Image: "img"}},
	}
	c := fake.NewClientBuilder().WithScheme(autoUpdateScheme(t)).WithObjects(pool).Build()
	// A registry that would error if consulted proves untracked pools are skipped
	// before any check.
	reconcileOpenclaw(t, c, fakeRegistry{err: context.Canceled})
}

func TestRebasePlan(t *testing.T) {
	cases := []struct {
		action     string
		autoRebase bool
		offer      bool
	}{
		{string(runmanifest.ResnapshotAutoRebase), true, false},
		{string(runmanifest.ResnapshotOfferRebase), false, true},
		{string(runmanifest.ResnapshotOnly), false, false},
		{"", false, true},        // default is offer
		{"garbage", false, true}, // unrecognized is conservative: offer
	}
	for _, c := range cases {
		got := rebasePlan(c.action)
		if got.AutoRebase != c.autoRebase || got.Offer != c.offer {
			t.Errorf("rebasePlan(%q) = %+v, want auto=%v offer=%v", c.action, got, c.autoRebase, c.offer)
		}
	}
}
