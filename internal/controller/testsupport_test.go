package controller

// Test support: used by envtest suites. Kept in the main package so external
// test packages (controller_test) can start fake forkd nodes.

import (
	"context"
	"crypto/tls"
	v1 "mitos.run/mitos/api/v1"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/eventfeed"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/observability"
	"mitos.run/mitos/internal/workspace"
	forkdpb "mitos.run/mitos/proto/forkd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// TraceIDAnnotationsForTest exposes traceIDAnnotations to the external
// controller_test package so the trace-id stamp omit branch (tracing off ->
// nil, no fake id) can be unit-tested deterministically without the OTel global
// provider one-time-delegate gotcha.
func TraceIDAnnotationsForTest(ctx context.Context) map[string]string {
	return traceIDAnnotations(ctx)
}

// BuildHuskPodForTest exposes buildHuskPod to the external controller_test
// package so the husk pod spec can be unit-tested.
func (r *SandboxPoolReconciler) BuildHuskPodForTest(pool *v1.SandboxPool, template *v1.PoolTemplateSpec, opts HuskPodOptions) *corev1.Pod {
	return r.buildHuskPod(pool, template, opts)
}

// BuildForkChildPodForTest exposes buildForkChildPod to the external
// controller_test package so the fork child pod spec can be unit-tested.
func BuildForkChildPodForTest(fork *v1.Sandbox, childName string, opts HuskPodOptions, scheme *runtime.Scheme) *corev1.Pod {
	return buildForkChildPod(fork, childName, opts, scheme)
}

// SetForkSnapshotForTest installs the fork-snapshot seam (tests only).
func (r *SandboxReconciler) SetForkSnapshotForTest(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error)) {
	r.forkSnapshot = fn
}

// SetForkSnapshotRemoverForTest installs the remove seam (tests only).
func (r *SandboxReconciler) SetForkSnapshotRemoverForTest(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error)) {
	r.removeForkSnapshot = fn
}

// SkipForkLabel restricts the reconciler to sandboxes WITHOUT the given label,
// for the husk-fork harness; an alias of SkipLabel on the consolidated reconciler.
func (r *SandboxReconciler) SkipForkLabel(label string) { r.skipLabel(label, "sandbox-fork-raw") }

// OnlyForkLabel restricts the reconciler to sandboxes WITH the given label.
func (r *SandboxReconciler) OnlyForkLabel(label string) { r.onlyLabel(label, "sandbox-fork-husk") }

// HuskTestClaimLabel marks a Sandbox as owned by the husk-activation tests. The
// suite registers the raw reconciler to SKIP these (so it does not fight a
// manually driven husk reconciler over the same object) and a husk-enabled
// reconciler to handle ONLY these.
const HuskTestClaimLabel = "mitos.run/husk-test"

// HuskForkTestLabel marks a Sandbox as owned by the husk-fork tests.
const HuskForkTestLabel = "mitos.run/husk-fork-test"

// SkipLabel restricts this reconciler to sandboxes WITHOUT the given label; only
// used by the test harness so a raw and a husk reconciler can share one manager.
func (r *SandboxReconciler) SkipLabel(label string) { r.skipLabel(label, "sandbox-raw") }

// OnlyLabel restricts this reconciler to sandboxes WITH the given label.
func (r *SandboxReconciler) OnlyLabel(label string) { r.onlyLabel(label, "sandbox-husk") }

func (r *SandboxReconciler) skipLabel(label, name string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[label] == ""
	})
	r.controllerName = name
}

// SkipLabels restricts this reconciler to sandboxes carrying NONE of the given
// labels (the raw reconciler skips every husk-driven object so it does not fight
// the husk reconcilers on the consolidated single kind).
func (r *SandboxReconciler) SkipLabels(labels ...string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		for _, l := range labels {
			if o.GetLabels()[l] != "" {
				return false
			}
		}
		return true
	})
	r.controllerName = "sandbox-raw"
}

func (r *SandboxReconciler) onlyLabel(label, name string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[label] != ""
	})
	r.controllerName = name
}

// OnlyLabels restricts this reconciler to sandboxes carrying ANY of the given
// labels. With the consolidated single kind, one husk reconciler handles both the
// husk-claim and husk-fork test sandboxes (it owns both engines), so the husk pod
// watch enqueues each sandbox on exactly one reconciler.
func (r *SandboxReconciler) OnlyLabels(labels ...string) {
	r.eventFilter = predicate.NewPredicateFuncs(func(o client.Object) bool {
		for _, l := range labels {
			if o.GetLabels()[l] != "" {
				return true
			}
		}
		return false
	})
	r.controllerName = "sandbox-husk"
}

// SetActivateForTest injects a fake husk activator (the test seam).
func (r *SandboxReconciler) SetActivateForTest(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)) {
	r.Activate = fn
}

// SetFeedForTest wires the change feed (the Kubernetes Event recorder, the
// CloudEvents sink, and a pinned clock) so envtest can assert both the Event
// mirror and the CloudEvents emit without a real webhook or wall clock.
func (r *SandboxReconciler) SetFeedForTest(recorder record.EventRecorder, sink eventfeed.Sink, clock func() time.Time) {
	r.Feed = NewEmitFeed(recorder, sink, clock)
}

// EmitRevisionCreatedForTest exposes emitRevisionCreated to the external
// controller_test package so the revision.created payload mapping (including the
// mitos.run/trace-id annotation -> TraceID correlation field) can be
// unit-tested directly against a recording sink, without a full reconcile.
func EmitRevisionCreatedForTest(recorder record.EventRecorder, sink eventfeed.Sink, rev *v1.WorkspaceRevision) {
	NewEmitFeed(recorder, sink, nil).emitRevisionCreated(context.Background(), rev)
}

// SetCheckpointForTest injects a fake live-VM checkpointer (the drain seam).
// The fake records whether the Checkpoint drain policy routed through it and
// returns the scripted captured/error. nil restores the default.
func (r *SandboxReconciler) SetCheckpointForTest(fn func(ctx context.Context, claim *v1.Sandbox, pod *corev1.Pod) (bool, error)) {
	r.Checkpoint = fn
}

// SetWorkspaceTransferForTest injects the workspace hydrate/dehydrate/diff/git
// seams so envtest can drive the binding lifecycle without a VM. hydrate records
// the manifest it was asked to restore; dehydrate returns a scripted digest and
// records the exclude and capture lists it was passed; diff returns a scripted
// content diff; rendezvous records the git push it was asked to make. A nil diff
// or rendezvous leaves the production default in place.
func (r *SandboxReconciler) SetWorkspaceTransferForTest(
	hydrate func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error),
	diff func(ctx context.Context, claim *v1.Sandbox, parent, child cas.Digest) (workspace.Diff, error),
	rendezvous func(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *workspace.Credentials) error,
	repoFiles func(ctx context.Context, claim *v1.Sandbox, digest cas.Digest, gitPaths []string) (map[string]string, error),
) {
	r.HydrateWorkspace = hydrate
	r.DehydrateWorkspace = dehydrate
	r.DiffWorkspace = diff
	r.RendezvousGit = rendezvous
	r.RepoFilesForGit = repoFiles
}

// SetWorkspaceDelegateForTest injects the husk-mode workspace transport
// delegates (the dial-the-husk-pod hydrate/dehydrate ops) so envtest can prove
// the husk reconciler routes its default hydrate/dehydrate through the delegate
// (and the controller still commits the revision + advances the head) without a
// real husk pod or vsock. hydrate records the manifest it was asked to restore;
// dehydrate returns a scripted digest and records the excludes/captures.
func (r *SandboxReconciler) SetWorkspaceDelegateForTest(
	hydrate func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error),
) {
	r.WorkspaceHydrateDelegate = hydrate
	r.WorkspaceDehydrateDelegate = dehydrate
}

// SetWorkspaceDehydrateDiffDelegateForTest injects the husk-mode combined
// dehydrate+diff delegate (the single node-CAS-side op that captures the workspace
// and, when wantDiff is set, computes the diff against the parent head). It lets
// envtest prove the husk terminate path commits a revision WITH a diff summary
// without a real husk pod or node CAS, and that the diff never routes through the
// in-controller workspaceTransport seam.
func (r *SandboxReconciler) SetWorkspaceDehydrateDiffDelegateForTest(
	fn func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string, parentManifest cas.Digest, wantDiff bool) (cas.Digest, *workspace.Diff, error),
) {
	r.WorkspaceDehydrateDiffDelegate = fn
}

// MemSnapshotResultForTest is the exported alias of the unexported
// memSnapshotResult so the external controller_test package can name the
// checkpoint fake's return type.
type MemSnapshotResultForTest = memSnapshotResult

// NewMemSnapshotResult builds a MemSnapshotResultForTest for tests.
func NewMemSnapshotResult(ref, principal string) MemSnapshotResultForTest {
	return memSnapshotResult{Ref: ref, Principal: principal}
}

// SetMemorySnapshotForTest injects the memory-snapshot pairing seams (W4 Task
// 2): the checkpoint-on-terminate capture, the resume-on-activate restore, and
// the principal-bound existence check. envtest drives the pairing decision and
// the resume/hydrate request without a real VM.
func (r *SandboxReconciler) SetMemorySnapshotForTest(
	checkpoint func(ctx context.Context, claim *v1.Sandbox) (MemSnapshotResultForTest, error),
	resume func(ctx context.Context, claim *v1.Sandbox, ref string) error,
	exists func(ctx context.Context, ref, principal string) (bool, error),
) {
	r.CheckpointMemory = checkpoint
	r.ResumeMemory = resume
	r.MemorySnapshotExists = exists
}

// SetSnapshotExistsForTest injects the workspace reconciler's resumable
// existence check so a test can flip a head's snapshot present/absent.
func (r *WorkspaceReconciler) SetSnapshotExistsForTest(exists func(ctx context.Context, ref, principal string) (bool, error)) {
	r.SnapshotExists = exists
}

// EnsureHuskPDBForTest exposes ensureHuskPDB to the external controller_test
// package so the PDB create-or-update can be envtested directly.
func (r *SandboxPoolReconciler) EnsureHuskPDBForTest(ctx context.Context, pool *v1.SandboxPool) error {
	return r.ensureHuskPDB(ctx, pool)
}

// EnsureHuskNetworkPolicyForTest exposes ensureHuskNetworkPolicy to the external
// controller_test package so the best-effort NetworkPolicy create-or-update can
// be envtested directly.
func (r *SandboxPoolReconciler) EnsureHuskNetworkPolicyForTest(ctx context.Context, pool *v1.SandboxPool, allow []string) error {
	return r.ensureHuskNetworkPolicy(ctx, pool, allow)
}

// HuskNetworkPolicyNameForTest exposes huskNetworkPolicyName so the external
// controller_test package can look the object up by name.
func HuskNetworkPolicyNameForTest(pool string) string { return huskNetworkPolicyName(pool) }

// ReconcileHuskPodsForTest exposes reconcileHuskPods to the external
// controller_test package so the warm-pool lifecycle can be envtested.
func (r *SandboxPoolReconciler) ReconcileHuskPodsForTest(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec) (int32, error) {
	res, err := r.reconcileHuskPods(ctx, pool, template, poolWarmMin(pool))
	return res.dormant, err
}

// EnsureTemplateBuiltForTest exposes ensureTemplateBuilt to the external
// controller_test package so the husk-mode "build the snapshot first" half can
// be envtested without driving the full Reconcile (which would race the
// manager's pool reconciler on the pool status subresource).
func (r *SandboxPoolReconciler) EnsureTemplateBuiltForTest(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec) error {
	nodeFilter, err := r.placementFilter(ctx, pool)
	if err != nil {
		return err
	}
	return r.ensureTemplateBuilt(ctx, pool, template, nodeFilter)
}

// EncKeyRecorder records, per RPC, the length of any EncryptionKey the fake
// forkd received. It records presence/length only, NEVER the key value, so a
// test can assert the controller delivered a key without the value ever
// touching test state or logs.
type EncKeyRecorder struct {
	mu            sync.Mutex
	createKeyLen  int
	createKeySeen bool
	createKekID   string
	forkKeyLen    int
	forkKeySeen   bool
	forkKekID     string
}

// CreateTemplateKeyLen returns whether a CreateTemplate carried an encryption
// key (the wrapped DEK) and its length.
func (r *EncKeyRecorder) CreateTemplateKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createKeySeen, r.createKeyLen
}

// CreateTemplateKekID returns the KEK id a CreateTemplate carried (non-secret).
func (r *EncKeyRecorder) CreateTemplateKekID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createKekID
}

// ForkKeyLen returns whether a Fork carried an encryption key (the wrapped DEK)
// and its length.
func (r *EncKeyRecorder) ForkKeyLen() (seen bool, length int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forkKeySeen, r.forkKeyLen
}

// ForkKekID returns the KEK id a Fork carried (non-secret).
func (r *EncKeyRecorder) ForkKekID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forkKekID
}

func (r *EncKeyRecorder) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		switch m := req.(type) {
		case *forkdpb.CreateTemplateRequest:
			r.mu.Lock()
			r.createKeySeen = true
			r.createKeyLen = len(m.EncryptionKey)
			r.createKekID = m.KekId
			r.mu.Unlock()
		case *forkdpb.ForkRequest:
			r.mu.Lock()
			r.forkKeySeen = true
			r.forkKeyLen = len(m.EncryptionKey)
			r.forkKekID = m.KekId
			r.mu.Unlock()
		}
		return handler(ctx, req)
	}
}

// StartFakeForkdNodeEncRecording is StartFakeForkdNode that also installs an
// EncKeyRecorder so a test can assert whether the controller delivered an
// encryption key in CreateTemplate and Fork (presence/length only, not the
// value).
func StartFakeForkdNodeEncRecording(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), rec *EncKeyRecorder, err error) {
	rec = &EncKeyRecorder{}
	stop, err = startFakeForkdNodeWithInterceptor(registry, nodeName, rec.interceptor(), templates...)
	return stop, rec, err
}

// StartFakeForkdNodeEncRecordingTLS is StartFakeForkdNodeEncRecording over
// mTLS: the gRPC listener is terminated by serverTLS and the registered
// NodeInfo carries clientTLS, so dials to THIS node use TLS. The encryption key
// delivery guard requires an mTLS node, so the happy-path enc tests run here.
func StartFakeForkdNodeEncRecordingTLS(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), rec *EncKeyRecorder, err error) {
	rec = &EncKeyRecorder{}
	stop, _, _, err = startFakeForkdNodeOpts(registry, nodeName, serverTLS, clientTLS, rec.interceptor(), templates...)
	return stop, rec, err
}

// StartFakeForkdNode runs an in-process forkd gRPC server backed by a
// MockEngine with the given templates, registers it in the registry, and
// returns a stop function.
func StartFakeForkdNode(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, nil, nil, templates...)
	return stop, err
}

// StartFakeForkdNodeRecording is StartFakeForkdNode that also returns the
// backing MockEngine, so tests can read engine.TerminatedIDs() to assert a
// VM was reaped via forkd Terminate, and a setActivity closure that stamps a
// sandbox's last-activity time on the node's SandboxAPI (for idle-reap tests).
func StartFakeForkdNodeRecording(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(sandboxID string, t time.Time), err error) {
	return startFakeForkdNode(registry, nodeName, nil, nil, templates...)
}

// StartFakeForkdNodeTLS is StartFakeForkdNode with mTLS: the gRPC listener
// is terminated by serverTLS and the registered NodeInfo carries clientTLS,
// so only dials to THIS node use TLS; other registered fakes stay insecure.
func StartFakeForkdNodeTLS(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNode(registry, nodeName, serverTLS, clientTLS, templates...)
	return stop, err
}

// startFakeForkdNodeWithInterceptor starts a fake forkd node with an extra
// unary server interceptor (used to record the request-delivered encryption
// key) and otherwise behaves like StartFakeForkdNode.
func startFakeForkdNodeWithInterceptor(registry *NodeRegistry, nodeName string, interceptor grpc.UnaryServerInterceptor, templates ...string) (stop func(), err error) {
	stop, _, _, err = startFakeForkdNodeOpts(registry, nodeName, nil, nil, interceptor, templates...)
	return stop, err
}

func startFakeForkdNode(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), err error) {
	return startFakeForkdNodeOpts(registry, nodeName, serverTLS, clientTLS, nil, templates...)
}

// StartFakeForkdNodeWithAPI is StartFakeForkdNodeRecording that also returns the
// node's SandboxAPI, so a test can inject the work-aware idle signals (issue
// #218) the controller reads through ListSandboxes: MarkPaused to hold a
// sandbox's clock, or SetTimeout to set a live TTL deadline. It lets the idle
// reaper's work-awareness be driven end to end on the envtest/mock path.
func StartFakeForkdNodeWithAPI(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), engine *fork.MockEngine, api *daemon.SandboxAPI, err error) {
	return startFakeForkdNodeWithAPI(registry, nodeName, templates...)
}

func startFakeForkdNodeOpts(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, interceptor grpc.UnaryServerInterceptor, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), err error) {
	stop, engine, setActivity, _, err = startFakeForkdNodeFull(registry, nodeName, serverTLS, clientTLS, interceptor, templates...)
	return stop, engine, setActivity, err
}

// startFakeForkdNodeWithAPI runs a fake forkd node and returns the node's
// SandboxAPI so a test can inject the work-aware idle signals the controller
// reads through ListSandboxes (issue #218).
func startFakeForkdNodeWithAPI(registry *NodeRegistry, nodeName string, templates ...string) (stop func(), engine *fork.MockEngine, api *daemon.SandboxAPI, err error) {
	stop, engine, _, api, err = startFakeForkdNodeFull(registry, nodeName, nil, nil, nil, templates...)
	return stop, engine, api, err
}

func startFakeForkdNodeFull(registry *NodeRegistry, nodeName string, serverTLS, clientTLS *tls.Config, interceptor grpc.UnaryServerInterceptor, templates ...string) (stop func(), engine *fork.MockEngine, setActivity func(string, time.Time), api *daemon.SandboxAPI, err error) {
	engine = fork.NewMockEngine()
	engine.ForkDelay = 0
	for _, tmpl := range templates {
		if err := engine.CreateTemplate(tmpl, tmpl, nil, nil, nil, nil); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	dir, err := os.MkdirTemp("", "fake-forkd-*")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	sandboxAPI := daemon.NewSandboxAPI(dir)
	srv := daemon.NewServer(engine, sandboxAPI)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, nil, nil, err
	}
	// The otelgrpc server handler mirrors forkd's real gRPC server so the
	// propagated trace context is honored: the forkd.Fork span joins the
	// controller's trace, which the cross-process propagation test asserts.
	opts := []grpc.ServerOption{grpc.StatsHandler(observability.GRPCServerStatsHandler())}
	if serverTLS != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	if interceptor != nil {
		opts = append(opts, grpc.UnaryInterceptor(interceptor))
	}
	gs := grpc.NewServer(opts...)
	daemon.RegisterForkDaemonServer(gs, srv)
	go gs.Serve(lis)

	// Real HTTP sandbox API on a real listener, exactly the handler forkd
	// serves on :9091, so envtest claims can exercise bearer-token auth
	// end to end against the registered HTTPEndpoint.
	httpSrv := httptest.NewServer(sandboxAPI.Handler())

	registry.Register(&NodeInfo{
		Name:         nodeName,
		Endpoint:     lis.Addr().String(),
		HTTPEndpoint: strings.TrimPrefix(httpSrv.URL, "http://"),
		TemplateIDs:  templates,
		MaxSandboxes: 100,
		TLS:          clientTLS,
	})
	setActivity = func(sandboxID string, t time.Time) {
		sandboxAPI.RecordActivity(sandboxID, t)
	}
	return func() {
		gs.Stop()
		httpSrv.Close()
		os.RemoveAll(dir)
		registry.Unregister(nodeName)
	}, engine, setActivity, sandboxAPI, nil
}
