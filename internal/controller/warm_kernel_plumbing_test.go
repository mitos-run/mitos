package controller

// Plumbing tests for the pool template's warmKernel opt-in: the controller
// must carry it into the forkd CreateTemplate RPC on every build path (the
// deficit fill via createSnapshotsOnNodes, the shared buildTemplateOnNode
// primitive, and the spec-driven rebuild via rebuildStaleSnapshots), where the
// node runs the pre-snapshot kernel warmup. The forkd node is real (a daemon
// server over gRPC); the engine is the mock, which records the flag.

import (
	"context"
	"testing"

	v1 "mitos.run/mitos/api/v1"
)

func TestBuildTemplateOnNodePlumbsWarmKernel(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry}
	engine := startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092")

	if err := r.buildTemplateOnNode(context.Background(), "node-a", "tmpl-warm", "python:3.12-slim", nil, nil, nil, "", nil, nil, false, true); err != nil {
		t.Fatalf("buildTemplateOnNode(warm): %v", err)
	}
	if !engine.LastWarmKernel() {
		t.Error("expected warm_kernel=true to reach the engine through the CreateTemplate RPC")
	}

	if err := r.buildTemplateOnNode(context.Background(), "node-a", "tmpl-cold", "python:3.12-slim", nil, nil, nil, "", nil, nil, false, false); err != nil {
		t.Fatalf("buildTemplateOnNode(cold): %v", err)
	}
	if engine.LastWarmKernel() {
		t.Error("expected warm_kernel=false to reach the engine when the pool does not opt in")
	}
}

func TestCreateSnapshotsOnNodesPlumbsWarmKernel(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry}
	engine := startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092")

	added, err := r.createSnapshotsOnNodes(context.Background(), "T", "img", nil, nil, nil, "", 1, nil, nil, nil, true)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if !engine.LastWarmKernel() {
		t.Error("expected the deficit-fill build to carry warm_kernel=true")
	}
}

// TestRebuildStaleSnapshotsCarriesTemplateWarmKernel proves the SPEC mapping:
// a PoolTemplateSpec with warmKernel: true reaches the node on the rebuild
// path, which reads the flag straight off the template spec.
func TestRebuildStaleSnapshotsCarriesTemplateWarmKernel(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry}
	engine := startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092", "T")

	template := &v1.PoolTemplateSpec{Image: "python:3.12-slim", WarmKernel: true}
	rebuilt, err := r.rebuildStaleSnapshots(context.Background(), "T", template, nil, "", nil, true)
	if err != nil {
		t.Fatalf("rebuildStaleSnapshots: %v", err)
	}
	if rebuilt != 1 {
		t.Fatalf("rebuilt = %d, want 1", rebuilt)
	}
	if !engine.LastWarmKernel() {
		t.Error("expected template.warmKernel=true to reach the engine on the rebuild path")
	}
}
