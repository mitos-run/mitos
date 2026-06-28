package controlplane

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/apierr"
)

// TestResolveRuntimeOwnedReturnsTarget asserts a Ready, org-owned sandbox resolves
// to its endpoint, per-sandbox token, and name.
func TestResolveRuntimeOwnedReturnsTarget(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-pty", "10.1.2.3:9091", "the-real-token")
	cp := New(newFakeClient(t, sb, secret))

	target, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-pty")
	if aerr != nil {
		t.Fatalf("ResolveRuntime: unexpected error %+v", *aerr)
	}
	if target.Endpoint != "10.1.2.3:9091" {
		t.Errorf("endpoint = %q", target.Endpoint)
	}
	if target.Token != "the-real-token" {
		t.Errorf("token = %q, want the per-sandbox token", target.Token)
	}
	if target.SandboxID != "sb-pty" {
		t.Errorf("sandbox id = %q", target.SandboxID)
	}
}

// TestResolveRuntimeCrossOrgIsNotFound asserts a sandbox owned by another org is
// not_found, never resolved, so the WebSocket proxy cannot reach it and another
// org's existence is never revealed.
func TestResolveRuntimeCrossOrgIsNotFound(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-pty", "10.1.2.3:9091", "the-real-token")
	cp := New(newFakeClient(t, sb, secret))

	_, aerr := cp.ResolveRuntime(context.Background(), "org-b", "sb-pty")
	if aerr == nil {
		t.Fatal("expected not_found for a cross-org id, got a target")
	}
	if aerr.Code != string(apierr.CodeNotFound) {
		t.Fatalf("code = %q, want not_found", aerr.Code)
	}
}

// TestResolveRuntimeMissingIsNotFound asserts an unknown id is not_found.
func TestResolveRuntimeMissingIsNotFound(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-pty", "10.1.2.3:9091", "the-real-token")
	cp := New(newFakeClient(t, sb, secret))

	_, aerr := cp.ResolveRuntime(context.Background(), orgA, "does-not-exist")
	if aerr == nil || aerr.Code != string(apierr.CodeNotFound) {
		t.Fatalf("want not_found, got %+v", aerr)
	}
}

// TestResolveRuntimeNotReadyIsNotFound asserts a sandbox with no runtime endpoint
// (not Ready) is not_found rather than resolving to an empty backend.
func TestResolveRuntimeNotReadyIsNotFound(t *testing.T) {
	sb, secret := readySandbox(orgA, "sb-pty", "10.1.2.3:9091", "the-real-token")
	sb.Status.Endpoint = ""
	cp := New(newFakeClient(t, sb, secret))

	_, aerr := cp.ResolveRuntime(context.Background(), orgA, "sb-pty")
	if aerr == nil || aerr.Code != string(apierr.CodeNotFound) {
		t.Fatalf("want not_found for a not-Ready sandbox, got %+v", aerr)
	}
}
