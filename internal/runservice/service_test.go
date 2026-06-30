package runservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/runmanifest"
)

const openclawYAML = `
name: openclaw
title: OpenClaw
source:
  image: ghcr.io/openclaw/openclaw:latest
run:
  command: ["node", "openclaw.mjs"]
preview:
  port: 18789
  auth: ladder
secrets:
  - { name: OPENCLAW_GATEWAY_TOKEN, label: Gateway token, generate: 32 }
  - { name: ANTHROPIC_API_KEY, label: Claude API key, required: true }
egress:
  allow: [api.anthropic.com]
workspace:
  path: /home/node/.openclaw
  persist: true
`

func mustManifest(t *testing.T, y string) *runmanifest.Manifest {
	t.Helper()
	m, err := runmanifest.Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return m
}

type fakeFetcher struct {
	m   *runmanifest.Manifest
	err error
}

func (f *fakeFetcher) Fetch(context.Context, string) (*runmanifest.Manifest, error) {
	return f.m, f.err
}

type fakeApplier struct {
	applied []client.Object
	err     error
}

func (f *fakeApplier) Apply(_ context.Context, objs ...client.Object) error {
	if f.err != nil {
		return f.err
	}
	f.applied = append(f.applied, objs...)
	return nil
}

func TestDescribe(t *testing.T) {
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, &fakeApplier{}, "mitos.run")
	d, err := svc.Describe(context.Background(), "github.com/openclaw/openclaw")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.Name != "openclaw" || d.Image != "ghcr.io/openclaw/openclaw:latest" {
		t.Errorf("description = %+v", d)
	}
	if len(d.Secrets) != 2 {
		t.Fatalf("secrets = %d, want 2", len(d.Secrets))
	}
	// The consent contract carries prompts and flags, never values.
	var req, gen bool
	for _, s := range d.Secrets {
		if s.Name == "ANTHROPIC_API_KEY" && s.Required {
			req = true
		}
		if s.Name == "OPENCLAW_GATEWAY_TOKEN" && s.Generate {
			gen = true
		}
	}
	if !req || !gen {
		t.Errorf("expected required+generate flags surfaced: %+v", d.Secrets)
	}
	if len(d.Egress) != 1 || d.Egress[0] != "api.anthropic.com" {
		t.Errorf("egress = %v", d.Egress)
	}
}

func TestRunProvisionsAndApplies(t *testing.T) {
	ap := &fakeApplier{}
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, ap, "mitos.run")
	res, err := svc.Run(context.Background(), "github.com/openclaw/openclaw",
		Identity{Namespace: "tenant-jannes", InstanceLabel: "jannes-openclaw"},
		map[string]string{"ANTHROPIC_API_KEY": "sk-real"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.URL != "https://jannes-openclaw.mitos.run" {
		t.Errorf("URL = %q", res.URL)
	}

	// The golden pool, the Secret, and the Sandbox are all applied, into the
	// tenant namespace.
	if len(ap.applied) != 3 {
		t.Fatalf("applied %d objects, want 3", len(ap.applied))
	}
	var pool, secret, sandbox bool
	for _, o := range ap.applied {
		if o.GetNamespace() != "tenant-jannes" {
			t.Errorf("object %T in namespace %q, want tenant-jannes", o, o.GetNamespace())
		}
		switch obj := o.(type) {
		case *v1.SandboxPool:
			pool = true
		case *corev1.Secret:
			secret = true
			if string(obj.Data["ANTHROPIC_API_KEY"]) != "sk-real" {
				t.Error("supplied secret missing from applied Secret")
			}
		case *v1.Sandbox:
			sandbox = true
			if obj.Spec.Source.PoolRef == nil || obj.Spec.Source.PoolRef.Name != "openclaw" {
				t.Error("sandbox does not fork the golden pool")
			}
		}
	}
	if !pool || !secret || !sandbox {
		t.Errorf("applied set incomplete: pool=%v secret=%v sandbox=%v", pool, secret, sandbox)
	}
}

// TestRunThreadsPublicURL asserts Run resolves the public URL and threads it
// through: the shared golden pool gets the stable canonical URL
// (https://<pool>.<domain>) at build time, while the per-fork Sandbox gets its own
// per-instance URL (https://<label>.<domain>) in its env.
func TestRunThreadsPublicURL(t *testing.T) {
	ap := &fakeApplier{}
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, ap, "mitos.run")
	if _, err := svc.Run(context.Background(), "github.com/openclaw/openclaw",
		Identity{Namespace: "tenant-jannes", InstanceLabel: "jannes-openclaw"},
		map[string]string{"ANTHROPIC_API_KEY": "sk-real"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var pool *v1.SandboxPool
	var sandbox *v1.Sandbox
	for _, o := range ap.applied {
		switch obj := o.(type) {
		case *v1.SandboxPool:
			pool = obj
		case *v1.Sandbox:
			sandbox = obj
		}
	}
	if pool == nil || sandbox == nil {
		t.Fatalf("missing applied objects: pool=%v sandbox=%v", pool != nil, sandbox != nil)
	}
	const wantGolden = "https://openclaw.mitos.run"
	if got := envValue(pool.Spec.Template.Env, runmanifest.PublicURLEnvVar); got != wantGolden {
		t.Errorf("golden %s = %q, want %q", runmanifest.PublicURLEnvVar, got, wantGolden)
	}
	const wantInstance = "https://jannes-openclaw.mitos.run"
	if got := envValue(sandbox.Spec.Env, runmanifest.PublicURLEnvVar); got != wantInstance {
		t.Errorf("instance %s = %q, want %q", runmanifest.PublicURLEnvVar, got, wantInstance)
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestRunRequiredSecretMissing(t *testing.T) {
	ap := &fakeApplier{}
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, ap, "mitos.run")
	_, err := svc.Run(context.Background(), "x",
		Identity{Namespace: "ns", InstanceLabel: "inst"}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("want required-secret error, got %v", err)
	}
	if len(ap.applied) != 0 {
		t.Error("nothing should be applied when provisioning fails")
	}
}

func TestRunNeedsIdentity(t *testing.T) {
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, &fakeApplier{}, "mitos.run")
	if _, err := svc.Run(context.Background(), "x", Identity{}, nil); err == nil {
		t.Fatal("Run without identity should fail")
	}
}

func TestRunFetchError(t *testing.T) {
	svc := New(&fakeFetcher{err: errors.New("boom")}, &fakeApplier{}, "mitos.run")
	if _, err := svc.Run(context.Background(), "x",
		Identity{Namespace: "ns", InstanceLabel: "inst"}, nil); err == nil {
		t.Fatal("fetch error should propagate")
	}
}
