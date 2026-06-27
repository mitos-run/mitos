package controller

import (
	"context"
	"crypto/tls"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/pki"
)

// distMTLSPair builds a CA and the forkd server + controller client TLS configs
// so a distribution test can register mTLS fake nodes (the encrypted-build guard
// requires an mTLS node).
func distMTLSPair(t *testing.T) (serverTLS, clientTLS *tls.Config) {
	t.Helper()
	ca, err := pki.NewCA("mitos-test")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	serverLeaf, err := ca.Issue(pki.ServerName)
	if err != nil {
		t.Fatalf("issue server: %v", err)
	}
	clientLeaf, err := ca.Issue(pki.ControllerName)
	if err != nil {
		t.Fatalf("issue client: %v", err)
	}
	serverTLS, err = pki.ServerTLSConfig(serverLeaf.CertPEM, serverLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("server TLS: %v", err)
	}
	clientTLS, err = pki.ClientTLSConfig(clientLeaf.CertPEM, clientLeaf.KeyPEM, ca.CertPEM())
	if err != nil {
		t.Fatalf("client TLS: %v", err)
	}
	return serverTLS, clientTLS
}

// startFakeForkdNodeTLSDist is startFakeForkdNode with the gRPC listener
// terminated by serverTLS and the NodeInfo carrying clientTLS, so dials to this
// node use mTLS (NodeMTLS reports true).
func startFakeForkdNodeTLSDist(t *testing.T, registry *NodeRegistry, nodeName, httpEndpoint, casEndpoint string, serverTLS, clientTLS *tls.Config, heldTemplates ...string) *fork.MockEngine {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	digests := make(map[string]string)
	for _, tmpl := range heldTemplates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		digests[tmpl] = engine.GetCapacity().TemplateDigests[tmpl]
	}
	srv := daemon.NewServer(engine, daemon.NewSandboxAPI(t.TempDir()))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	registry.Register(&NodeInfo{
		Name:            nodeName,
		Endpoint:        lis.Addr().String(),
		HTTPEndpoint:    httpEndpoint,
		CASEndpoint:     casEndpoint,
		TemplateIDs:     heldTemplates,
		TemplateDigests: digests,
		MaxSandboxes:    100,
		TLS:             clientTLS,
	})
	return engine
}

// startFakeForkdNode runs a real forkd gRPC server backed by a MockEngine,
// registers it in the registry under nodeName with the given HTTP endpoint and
// optional pre-held templates (with fabricated digests), and returns the
// backing engine so a test can read recorded PullTemplate calls. The gRPC
// listener is insecure; distribution does not require the controller-to-forkd
// channel to be mTLS for a plaintext template.
func startDistForkdNode(t *testing.T, registry *NodeRegistry, nodeName, httpEndpoint, casEndpoint string, heldTemplates ...string) *fork.MockEngine {
	t.Helper()
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	digests := make(map[string]string)
	for _, tmpl := range heldTemplates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		digests[tmpl] = engine.GetCapacity().TemplateDigests[tmpl]
	}
	srv := daemon.NewServer(engine, daemon.NewSandboxAPI(t.TempDir()))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	registry.Register(&NodeInfo{
		Name:            nodeName,
		Endpoint:        lis.Addr().String(),
		HTTPEndpoint:    httpEndpoint,
		CASEndpoint:     casEndpoint,
		TemplateIDs:     heldTemplates,
		TemplateDigests: digests,
		MaxSandboxes:    100,
	})
	return engine
}

// TestDistributeBuildsOnceAndPulls is the core build-once-distribute proof: node
// A holds template T (reports its digest + HTTP endpoint) and node B lacks it.
// The reconcile must issue a PullTemplate to B sourced from A's CAS URL + the
// digest, and must NOT build a second time on B.
func TestDistributeBuildsOnceAndPulls(t *testing.T) {
	registry := NewNodeRegistry()
	const token = "peer-secret"
	r := &SandboxPoolReconciler{NodeRegistry: registry, PeerToken: token}

	engineA := startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092", "T")
	engineB := startDistForkdNode(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092")

	added, err := r.createSnapshotsOnNodes(context.Background(), "T", "img", nil, nil, nil, "", 1, nil, nil)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	// B must have pulled, not built. The mock records no CreateTemplate beyond
	// the seed; the pull is recorded with A's CAS URL + the digest.
	pulls := engineB.PullCalls()
	if len(pulls) != 1 {
		t.Fatalf("node B recorded %d pulls, want 1", len(pulls))
	}
	// The pull source is the holder's DEDICATED CAS endpoint (port 9092), NOT its
	// sandbox HTTP port: CAS distribution is served on its own TLS listener.
	wantURL := "https://10.0.0.1:9092/cas"
	wantDigest := engineA.GetCapacity().TemplateDigests["T"]
	if pulls[0].SourceURL != wantURL {
		t.Fatalf("pull source = %q, want %q", pulls[0].SourceURL, wantURL)
	}
	if pulls[0].ManifestDigest != wantDigest {
		t.Fatalf("pull digest = %q, want %q", pulls[0].ManifestDigest, wantDigest)
	}
	if pulls[0].TemplateID != "T" {
		t.Fatalf("pull template = %q, want T", pulls[0].TemplateID)
	}
	// The token reached forkd (length only; the value never touches test state).
	if pulls[0].TokenLen != len(token) {
		t.Fatalf("pull token length = %d, want %d", pulls[0].TokenLen, len(token))
	}
	// B did NOT build T (no second CreateTemplate): the mock only built its seed
	// templates, and B was started with none.
	if got := engineB.GetCapacity().TemplateIDs; len(got) != 1 || got[0] != "T" {
		t.Fatalf("node B templates = %v, want exactly [T] (from the pull)", got)
	}
}

// TestDistributeRespectsPlacementNodeFilter is the dedicatedNodes (#172) build
// constraint: a placed pool's husk pods are pinned to its placement nodes, so
// the template snapshot MUST be built only on those nodes. A snapshot on a node
// outside the placement set can never back a placement-pinned pod, so the build
// loop must skip non-placement nodes even when they are healthy and would
// otherwise satisfy the deficit. node-a is OUTSIDE the filter and node-b is
// INSIDE it; with a deficit of 2 the build must still land only on node-b.
func TestDistributeRespectsPlacementNodeFilter(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry, PeerToken: "peer-secret"}

	engineA := startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092")
	engineB := startDistForkdNode(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092")

	// Deficit 2, but only node-b is in the placement set.
	added, err := r.createSnapshotsOnNodes(context.Background(), "T", "img", nil, nil, nil, "", 2, map[string]bool{"node-b": true}, nil)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the placement node is eligible)", added)
	}
	// node-a is outside the placement set: it must NOT have built T.
	if got := engineA.GetCapacity().TemplateIDs; len(got) != 0 {
		t.Fatalf("node-a (outside placement) built %v, want nothing", got)
	}
	// node-b is the dedicated node: it must hold T.
	heldB := false
	for _, tid := range engineB.GetCapacity().TemplateIDs {
		if tid == "T" {
			heldB = true
		}
	}
	if !heldB {
		t.Fatalf("node-b (placement node) did not build T; templates=%v", engineB.GetCapacity().TemplateIDs)
	}
}

// TestReadySnapshotCountOnExcludesNonPlacementNodes proves the readiness count
// is placement-aware (#172): a snapshot held on a node OUTSIDE the placement set
// must not count toward a placed pool's ready snapshots, otherwise the
// readySnapshots>=desired gate would report the deficit met while no dedicated
// node holds the snapshot, and the placement-pinned husk pods would never get an
// eligible holder. A nil filter (unplaced pool) counts every healthy holder.
func TestReadySnapshotCountOnExcludesNonPlacementNodes(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry}

	// node-a holds T but is outside the placement set; node-b is inside it but
	// holds nothing.
	startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092", "T")
	startDistForkdNode(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092")

	if got := r.readySnapshotCountOn("T", map[string]bool{"node-b": true}); got != 0 {
		t.Fatalf("placement-aware count = %d, want 0 (the only holder is outside placement)", got)
	}
	if got := r.readySnapshotCountOn("T", nil); got != 1 {
		t.Fatalf("unfiltered count = %d, want 1", got)
	}
}

// TestDistributeNoHolderBuildsThenPulls proves that when NO node holds T, one
// node builds it (CreateTemplate) and the remaining deficit nodes pull from the
// freshly built holder in the same pass.
func TestDistributeNoHolderBuildsThenPulls(t *testing.T) {
	registry := NewNodeRegistry()
	const token = "peer-secret"
	r := &SandboxPoolReconciler{NodeRegistry: registry, PeerToken: token}

	// Three empty nodes, none holds T.
	engines := map[string]*fork.MockEngine{
		"node-a": startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092"),
		"node-b": startDistForkdNode(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092"),
		"node-c": startDistForkdNode(t, registry, "node-c", "10.0.0.3:9091", "10.0.0.3:9092"),
	}

	added, err := r.createSnapshotsOnNodes(context.Background(), "T", "img", nil, nil, nil, "", 3, nil, nil)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if added != 3 {
		t.Fatalf("added = %d, want 3", added)
	}

	// Exactly one node built T (its mock holds T but recorded no pull); the other
	// two pulled.
	builders, pullers := 0, 0
	for _, e := range engines {
		held := false
		for _, tid := range e.GetCapacity().TemplateIDs {
			if tid == "T" {
				held = true
			}
		}
		if !held {
			t.Fatalf("a node ended without template T")
		}
		if len(e.PullCalls()) == 0 {
			builders++
		} else {
			pullers++
		}
	}
	if builders != 1 || pullers != 2 {
		t.Fatalf("builders=%d pullers=%d, want 1 builder and 2 pullers", builders, pullers)
	}
}

// TestDistributeEncryptedBuildsEverywhere proves the encrypted carve-out: an
// encrypted template (encKey present) builds on every deficit node and never
// pulls, so plaintext CAS chunks never leave a node.
func TestDistributeEncryptedBuildsEverywhere(t *testing.T) {
	registry := NewNodeRegistry()
	r := &SandboxPoolReconciler{NodeRegistry: registry, PeerToken: "peer-secret"}

	// node A holds the encrypted template T; node B lacks it. mTLS is not
	// configured here, but an encrypted build on an insecure channel is refused
	// by the existing guard, so register both nodes with per-node TLS to allow
	// the build to proceed and prove no pull happens.
	serverTLS, clientTLS := distMTLSPair(t)
	engineA := startFakeForkdNodeTLSDist(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092", serverTLS, clientTLS, "T")
	engineB := startFakeForkdNodeTLSDist(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092", serverTLS, clientTLS)
	_ = engineA

	added, err := r.createSnapshotsOnNodes(context.Background(), "T", "img", nil, nil, []byte("0123456789abcdef0123456789abcdef"), "local:test", 1, nil, nil)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if len(engineB.PullCalls()) != 0 {
		t.Fatalf("encrypted template must not be distributed by pull; node B recorded %d pulls", len(engineB.PullCalls()))
	}
	// B built T (held after the build) rather than pulling it.
	held := false
	for _, tid := range engineB.GetCapacity().TemplateIDs {
		if tid == "T" {
			held = true
		}
	}
	if !held {
		t.Fatal("node B did not build the encrypted template T")
	}
}

// TestSnapshotRebuildsOnHolderNodeLoss is the raw-forkd pool's "rebuild
// elsewhere after node loss" guarantee at the level it actually exists: the
// SNAPSHOT distribution, not a pool of dormant VMs (a raw-forkd pool holds no
// standing VMs, only the per-node template snapshot). When a snapshot-holder
// node is lost, the deficit reconcile must redistribute the snapshot onto a
// surviving node to restore the desired replica count, with no operator action.
//
// This is the mock-achievable half of issue #163 item 6. The mechanism already
// exists (readySnapshotCountOn counts only healthy holders, so a lost holder
// drops the count and the next createSnapshotsOnNodes pass refills the deficit);
// this pins it with a test, which was the actual coverage gap.
func TestSnapshotRebuildsOnHolderNodeLoss(t *testing.T) {
	registry := NewNodeRegistry()
	const token = "peer-secret"
	r := &SandboxPoolReconciler{NodeRegistry: registry, PeerToken: token}
	ctx := context.Background()

	// node-a and node-b hold template T (two healthy holders); node-c is empty,
	// the replacement target.
	startDistForkdNode(t, registry, "node-a", "10.0.0.1:9091", "10.0.0.1:9092", "T")
	startDistForkdNode(t, registry, "node-b", "10.0.0.2:9091", "10.0.0.2:9092", "T")
	engineC := startDistForkdNode(t, registry, "node-c", "10.0.0.3:9091", "10.0.0.3:9092")

	if got := r.readySnapshotCountOn("T", nil); got != 2 {
		t.Fatalf("initial ready snapshots = %d, want 2 (node-a + node-b)", got)
	}

	// A holder node is lost. Unregister mirrors a node leaving the registry
	// (PruneStale / heartbeat-TTL expiry routes through the same healthy-set drop).
	registry.Unregister("node-a")
	if got := r.readySnapshotCountOn("T", nil); got != 1 {
		t.Fatalf("ready snapshots after node-a loss = %d, want 1", got)
	}

	// The next deficit reconcile (desired replicas 2, one holder left) must rebuild
	// the snapshot onto the surviving empty node-c, restoring the count to 2.
	deficit := int32(2) - r.readySnapshotCountOn("T", nil)
	added, err := r.createSnapshotsOnNodes(ctx, "T", "img", nil, nil, nil, "", deficit, nil, nil)
	if err != nil {
		t.Fatalf("createSnapshotsOnNodes after holder loss: %v", err)
	}
	if added != 1 {
		t.Fatalf("snapshots added after holder loss = %d, want 1 (redistributed to node-c)", added)
	}
	// node-c now holds T: it pulled from the surviving holder node-b (not a rebuild
	// from scratch), the build-once-distribute path.
	if len(engineC.PullCalls()) != 1 {
		t.Fatalf("node-c recorded %d pulls, want 1 (redistribute from a surviving holder)", len(engineC.PullCalls()))
	}
	if got := r.readySnapshotCountOn("T", nil); got != 2 {
		t.Fatalf("ready snapshots after rebuild = %d, want 2 (restored on a replacement node)", got)
	}
}

// TestRawForkdClaimAutoReplacementAfterNodeLossOpen documents, as an explicit
// skipped placeholder, the part of issue #163 item 6 that is NOT a missing test
// but a missing FEATURE and a deliberate open product decision (epic #12).
//
// In HUSK mode a Ready claim on a lost node re-pends onto a surviving dormant
// warm slot (checkHuskPodLost + the husk pod watch self-heal the warm pool). In
// RAW-FORKD mode there is no standing dormant capacity to re-pend onto: a
// raw-forkd pool holds only template snapshots, and per-claim forks are
// ephemeral. So a raw-forkd claim on a dead node is correctly marked NodeLost /
// Failed (proven by TestGCMarksNodeLost) and is NOT auto-replaced; the caller
// re-claims. This is acceptable open behavior for ephemeral sandboxes
// (docs/failure-gc.md), not a half-built mechanism, so this is a t.Skip, not a
// faked or failing assertion.
//
// Design IF a future product decision wants raw-mode auto-replacement: on a
// NodeLost transition in raw mode, the claim reconciler would re-issue the fork
// on a surviving snapshot-holder node (which TestSnapshotRebuildsOnHolderNodeLoss
// guarantees still exists or is rebuilt), rather than terminally failing the
// claim. It needs no KVM; it needs the product decision to give raw claims
// husk-like resilience at the cost of the pure fork-per-claim model.
func TestRawForkdClaimAutoReplacementAfterNodeLossOpen(t *testing.T) {
	t.Skip("#12: raw-forkd claim auto-replacement after NodeLost is an open product decision; today the claim is marked NodeLost and the caller re-claims (husk mode self-heals via the warm pool instead). Snapshot rebuild-elsewhere IS covered by TestSnapshotRebuildsOnHolderNodeLoss.")
}
