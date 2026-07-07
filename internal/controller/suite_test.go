package controller_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	v1 "mitos.run/mitos/api/v1"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/eventfeed"
	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/kms"
	"mitos.run/mitos/internal/workspace"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// recordingSink is the suite's fake CloudEvents sink: it records every emitted
// event so a test can assert the feed envelope and dedupe id without a real
// webhook. Concurrency-safe (the manager emits from reconcile goroutines).
type recordingSink struct {
	mu     sync.Mutex
	events []eventfeed.Event
}

func (s *recordingSink) Emit(_ context.Context, e eventfeed.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// byType returns the recorded events of the given CloudEvent type.
func (s *recordingSink) byType(t string) []eventfeed.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []eventfeed.Event
	for _, e := range s.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

var (
	// testSink records the feed CloudEvents the suite's raw claim reconciler
	// emits, so the binding/feed tests can assert the envelope and dedupe id.
	testSink = &recordingSink{}
	// testEventRecorder is the suite's Kubernetes Event recorder. It is a
	// non-blocking buffering recorder rather than record.NewFakeRecorder: the
	// FakeRecorder's channel BLOCKS the caller once full, and the suite emits one
	// Event per claim phase transition and per revision across every test on the
	// reconcile path, so a bounded channel would fill and stall reconciles (the
	// claims would time out). This recorder appends under a mutex and never
	// blocks; waitForEvent scans its snapshot.
	testEventRecorder = &bufferingRecorder{}
)

// bufferingRecorder is a non-blocking record.EventRecorder for the suite: it
// accumulates formatted events ("<type> <reason> <message>") under a mutex and
// never blocks the reconcile path. waitForEvent scans snapshot() for a match.
type bufferingRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *bufferingRecorder) record(eventtype, reason, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventtype+" "+reason+" "+msg)
}

func (r *bufferingRecorder) Event(_ runtime.Object, eventtype, reason, message string) {
	r.record(eventtype, reason, message)
}

func (r *bufferingRecorder) Eventf(_ runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.record(eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *bufferingRecorder) AnnotatedEventf(_ runtime.Object, _ map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.record(eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *bufferingRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// updateSandboxStatusWithRetry applies a Sandbox status mutation under
// RetryOnConflict, re-Getting the object each attempt. A test that Creates a
// sandbox and immediately stamps its status from the stale Create response
// loses the optimistic-lock race intermittently: the claim reconciler writes
// metadata (the pool-missing stamp) and status (a PoolNotFound pend) on a
// sandbox whose referenced pool does not exist, bumping the resourceVersion in
// between (issue #630).
func updateSandboxStatusWithRetry(t *testing.T, name, namespace string, mutate func(*v1.Sandbox)) {
	t.Helper()
	// retry.DefaultRetry is only ~5 short steps; a source claim whose reconciler
	// churns the object continuously (it re-reconciles while pending) can conflict
	// on every one of them, flaking the status write. A ~3s budget was measured to
	// still flake (the churn outlasts it), so retry for about 6.7s worst case
	// (20 steps, 40ms base, 1.3x, capped at 500ms), long enough to always catch a
	// gap in the reconcile loop while still bounding a genuinely wedged write.
	backoff := wait.Backoff{Steps: 20, Duration: 40 * time.Millisecond, Factor: 1.3, Cap: 500 * time.Millisecond}
	if err := retry.RetryOnConflict(backoff, func() error {
		var sb v1.Sandbox
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &sb); err != nil {
			return err
		}
		mutate(&sb)
		return k8sClient.Status().Update(ctx, &sb)
	}); err != nil {
		t.Fatalf("update sandbox %s/%s status: %v", namespace, name, err)
	}
}

var (
	testEnv      *envtest.Environment
	cfg          *rest.Config
	k8sClient    client.Client
	scheme       *runtime.Scheme
	ctx          context.Context
	cancel       context.CancelFunc
	testRegistry *controller.NodeRegistry
	// testKMS is the envelope-encryption Wrapper injected into the envtest
	// reconcilers: a local AES-256-GCM KEK so encrypted templates wrap their DEK
	// without cloud credentials. Initialized in TestMain.
	testKMS *kms.LocalKEK
	// logBuf accumulates the controller's log output for the whole suite so a
	// test can assert a secret value never appears in any log line. It is
	// concurrency-safe because the manager logs from reconcile goroutines.
	logBuf = &syncBuffer{}

	// huskTestActivatorMu guards the swappable husk activator the suite's
	// husk-enabled claim reconciler dials through. Tests set it via
	// setHuskTestActivator before creating their claim.
	huskTestActivatorMu sync.Mutex
	huskTestActivator   func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)

	// huskWSDelegateMu guards the swappable husk-mode workspace transport delegates
	// (the dial-the-husk-pod hydrate/dehydrate ops) the suite's husk claim
	// reconciler routes its default hydrate/dehydrate through. Tests set them via
	// setHuskWSDelegate to prove the husk reconciler delegates and the controller
	// still commits the revision + advances the head, without a real husk pod.
	huskWSDelegateMu sync.Mutex
	huskWSHydrate    func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error
	huskWSDehydrate  func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error)
	// huskWSDiff is the optional husk-mode diff fake the suite's
	// WorkspaceDehydrateDiffDelegate consults when an output requested a diff. nil
	// records no diff (the node returned none). A test that exercises the husk diff
	// path installs it via setHuskWSDiff; it stands in for the diff the husk-stub
	// computes from the two node-CAS manifests.
	huskWSDiff func(ctx context.Context, claim *v1.Sandbox, parentManifest, child cas.Digest) (workspace.Diff, error)

	// huskTestCheckpointerMu guards the swappable live-VM checkpointer the
	// suite's husk reconciler routes a Checkpoint drain policy through.
	huskTestCheckpointerMu sync.Mutex
	huskTestCheckpointer   func(ctx context.Context, claim *v1.Sandbox, pod *corev1.Pod) (bool, error)

	// forkSnapshotterMu guards the swappable husk fork-snapshot / activator /
	// remover seams the suite's husk-enabled fork reconciler dials through.
	forkSnapshotterMu   sync.Mutex
	forkSnapshotter     func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error)
	forkActivator       func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)
	forkSnapshotRemover func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error)
	forkVMSpawner       func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error)
	// forkMultiVMEnabled is the race-safe MultiVMFork toggle the suite's shared husk
	// reconciler reads through its gate; a MultiVMFork test flips it per-case.
	forkMultiVMEnabled bool

	// wsTransferMu guards the swappable workspace hydrate/dehydrate fakes the
	// suite's raw claim reconciler drives. Tests set them via setWSTransfer.
	wsTransferMu sync.Mutex
	wsHydrate    func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error
	wsDehydrate  func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error)
	wsDiff       func(ctx context.Context, claim *v1.Sandbox, parent, child cas.Digest) (workspace.Diff, error)
	wsRendezvous func(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *workspace.Credentials) error
	wsRepoFiles  func(ctx context.Context, claim *v1.Sandbox, digest cas.Digest, gitPaths []string) (map[string]string, error)

	// memSnapshotMu guards the swappable memory-snapshot pairing fakes (W4 Task
	// 2). Tests set them via setMemSnapshot before creating their claim/workspace.
	memSnapshotMu sync.Mutex
	memCheckpoint func(ctx context.Context, claim *v1.Sandbox) (controller.MemSnapshotResultForTest, error)
	memResume     func(ctx context.Context, claim *v1.Sandbox, ref string) error
	memExists     func(ctx context.Context, ref, principal string) (bool, error)
)

// MemSnapshotResultForTest is re-exported below for the suite's swappable fake
// signature; the real result type is unexported in the controller package.

// setMemSnapshot installs the memory-snapshot pairing fakes. nil for any leaves
// a safe default: checkpoint returns nothing captured, resume errors (so a test
// that expects resume but forgot to install it fails), exists returns false.
func setMemSnapshot(
	checkpoint func(ctx context.Context, claim *v1.Sandbox) (controller.MemSnapshotResultForTest, error),
	resume func(ctx context.Context, claim *v1.Sandbox, ref string) error,
	exists func(ctx context.Context, ref, principal string) (bool, error),
) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	memCheckpoint = checkpoint
	memResume = resume
	memExists = exists
}

func currentMemCheckpoint() func(ctx context.Context, claim *v1.Sandbox) (controller.MemSnapshotResultForTest, error) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memCheckpoint
}

func currentMemResume() func(ctx context.Context, claim *v1.Sandbox, ref string) error {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memResume
}

func currentMemExists() func(ctx context.Context, ref, principal string) (bool, error) {
	memSnapshotMu.Lock()
	defer memSnapshotMu.Unlock()
	return memExists
}

// setWSTransfer installs the workspace hydrate/dehydrate fakes; nil restores a
// default that fails closed so a test that forgot to set them does not silently
// pass.
func setWSTransfer(
	hydrate func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error),
) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsHydrate = hydrate
	wsDehydrate = dehydrate
}

// setWSDiff installs the workspace diff fake; nil falls back to a default that
// returns an empty diff so a test that does not exercise the diff path is
// unaffected.
func setWSDiff(diff func(ctx context.Context, claim *v1.Sandbox, parent, child cas.Digest) (workspace.Diff, error)) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsDiff = diff
}

// setWSRendezvous installs the git rendezvous fake; nil falls back to the
// production default (workspace.Rendezvous via the git CLI).
func setWSRendezvous(rv func(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *workspace.Credentials) error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsRendezvous = rv
}

// setWSRepoFiles installs the git repo-paths resolver fake; nil falls back to a
// default that resolves no files.
func setWSRepoFiles(fn func(ctx context.Context, claim *v1.Sandbox, digest cas.Digest, gitPaths []string) (map[string]string, error)) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	wsRepoFiles = fn
}

func currentWSRepoFiles() func(ctx context.Context, claim *v1.Sandbox, digest cas.Digest, gitPaths []string) (map[string]string, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsRepoFiles
}

func currentWSHydrate() func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsHydrate
}

func currentWSDehydrate() func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsDehydrate
}

func currentWSDiff() func(ctx context.Context, claim *v1.Sandbox, parent, child cas.Digest) (workspace.Diff, error) {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsDiff
}

func currentWSRendezvous() func(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *workspace.Credentials) error {
	wsTransferMu.Lock()
	defer wsTransferMu.Unlock()
	return wsRendezvous
}

// setHuskTestCheckpointer installs the checkpointer the suite reconciler uses
// for the Checkpoint drain policy; nil falls back to the default.
func setHuskTestCheckpointer(fn func(ctx context.Context, claim *v1.Sandbox, pod *corev1.Pod) (bool, error)) {
	huskTestCheckpointerMu.Lock()
	defer huskTestCheckpointerMu.Unlock()
	huskTestCheckpointer = fn
}

// currentHuskTestCheckpointer returns the installed checkpointer, or nil so the
// reconciler uses its default (re-pend without a captured snapshot).
func currentHuskTestCheckpointer() func(ctx context.Context, claim *v1.Sandbox, pod *corev1.Pod) (bool, error) {
	huskTestCheckpointerMu.Lock()
	defer huskTestCheckpointerMu.Unlock()
	return huskTestCheckpointer
}

// setHuskTestActivator installs the husk activator the suite reconciler uses.
func setHuskTestActivator(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)) {
	huskTestActivatorMu.Lock()
	defer huskTestActivatorMu.Unlock()
	huskTestActivator = fn
}

// currentHuskTestActivator returns the installed activator, or a default that
// fails closed (so a test that forgot to set one does not silently pass).
func currentHuskTestActivator() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	huskTestActivatorMu.Lock()
	defer huskTestActivatorMu.Unlock()
	if huskTestActivator == nil {
		return func(context.Context, string, *tls.Config, husk.ActivateRequest) (husk.ActivateResult, error) {
			return husk.ActivateResult{OK: false, Error: "no husk test activator installed"}, nil
		}
	}
	return huskTestActivator
}

// setHuskWSDelegate installs the husk-mode workspace transport delegates the
// suite husk claim reconciler routes its default hydrate/dehydrate through. nil
// for either leaves it unset; the delegate seam then defaults to the real
// dial-the-husk-pod path (which has no pod in envtest), so a test that exercises
// the workspace path must install both.
func setHuskWSDelegate(
	hydrate func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error,
	dehydrate func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error),
) {
	huskWSDelegateMu.Lock()
	defer huskWSDelegateMu.Unlock()
	huskWSHydrate = hydrate
	huskWSDehydrate = dehydrate
}

func currentHuskWSHydrate() func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error {
	huskWSDelegateMu.Lock()
	defer huskWSDelegateMu.Unlock()
	return huskWSHydrate
}

func currentHuskWSDehydrate() func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error) {
	huskWSDelegateMu.Lock()
	defer huskWSDelegateMu.Unlock()
	return huskWSDehydrate
}

// setHuskWSDiff installs the husk-mode diff fake the suite's combined
// dehydrate+diff delegate consults when an output requested a diff. It stands in
// for the diff the husk-stub computes from the two node-CAS manifests. nil clears
// it (no diff returned).
func setHuskWSDiff(fn func(ctx context.Context, claim *v1.Sandbox, parentManifest, child cas.Digest) (workspace.Diff, error)) {
	huskWSDelegateMu.Lock()
	defer huskWSDelegateMu.Unlock()
	huskWSDiff = fn
}

func currentHuskWSDiff() func(ctx context.Context, claim *v1.Sandbox, parentManifest, child cas.Digest) (workspace.Diff, error) {
	huskWSDelegateMu.Lock()
	defer huskWSDelegateMu.Unlock()
	return huskWSDiff
}

// setForkSnapshotter / currentForkSnapshotter swap the fork-snapshot seam the
// suite's husk fork reconciler dials through.
func setForkSnapshotter(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error)) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	forkSnapshotter = fn
}

func currentForkSnapshotter() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	if forkSnapshotter == nil {
		return func(context.Context, string, *tls.Config, husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
			return husk.ForkSnapshotResult{OK: false, Error: "no fork snapshotter installed"}, nil
		}
	}
	return forkSnapshotter
}

// setForkActivator / currentForkActivator swap the activate seam the suite's
// husk fork reconciler dials through.
func setForkActivator(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	forkActivator = fn
}

func currentForkActivator() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	if forkActivator == nil {
		return func(context.Context, string, *tls.Config, husk.ActivateRequest) (husk.ActivateResult, error) {
			return husk.ActivateResult{OK: false, Error: "no fork activator installed"}, nil
		}
	}
	return forkActivator
}

// forkActivatorInstalled reports whether a fork test installed a fork activator,
// so the shared husk activate seam can route a fork-child activation to it (and a
// husk-claim activation to the claim activator) on the one merged husk reconciler.
func forkActivatorInstalled() bool {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	return forkActivator != nil
}

// setForkSnapshotRemover / currentForkSnapshotRemover swap the remove-fork-snapshot
// seam the suite's husk fork reconciler dials through on delete.
func setForkSnapshotRemover(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error)) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	forkSnapshotRemover = fn
}

func currentForkSnapshotRemover() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	if forkSnapshotRemover == nil {
		return func(context.Context, string, *tls.Config, husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
			return husk.ForkSnapshotResult{OK: true}, nil
		}
	}
	return forkSnapshotRemover
}

// setForkVMSpawner / currentForkVMSpawner swap the spawn-vm seam the suite's husk
// reconciler dials through when the MultiVMFork routing spawns a child in the source
// pod.
func setForkVMSpawner(fn func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error)) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	forkVMSpawner = fn
}

func currentForkVMSpawner() func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	if forkVMSpawner == nil {
		return func(context.Context, string, *tls.Config, husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
			return husk.SpawnVMResult{OK: false, Error: "no fork vm spawner installed"}, nil
		}
	}
	return forkVMSpawner
}

// setForkMultiVM / currentForkMultiVM toggle the MultiVMFork routing on the suite's
// shared husk reconciler race-safely (a MultiVMFork test sets it per-case).
func setForkMultiVM(on bool) {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	forkMultiVMEnabled = on
}

func currentForkMultiVM() bool {
	forkSnapshotterMu.Lock()
	defer forkSnapshotterMu.Unlock()
	return forkMultiVMEnabled
}

// syncBuffer is a concurrency-safe io.Writer that accumulates everything
// written and lets a test snapshot the bytes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func TestMain(m *testing.M) {
	// Tee the controller logs into logBuf (and still to stderr) so secret-leak
	// assertions can scan everything the controller logged.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(io.MultiWriter(os.Stderr, logBuf))))

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	scheme = runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	// core/v1 too: the claim and fork reconcilers create token Secrets.
	_ = clientgoscheme.AddToScheme(scheme)

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "deploy", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	// Start controller manager in background
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		panic(err)
	}

	nodeRegistry := controller.NewNodeRegistry()
	testRegistry = nodeRegistry

	// A fixed local KEK so envelope encryption wraps DEKs in envtest without
	// cloud credentials. The KEK bytes are test-only and non-secret here.
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 3)
	}
	tk, kerr := kms.NewLocalKEK(kek)
	if kerr != nil {
		panic(kerr)
	}
	testKMS = tk

	// The suite's manager-level pool reconciler runs the raw-forkd path
	// explicitly (EnableHuskPods false). The husk-mode pool reconcile (build the
	// snapshot + create husk pods) is covered by a directly driven reconciler in
	// husk_pool_build_test.go, so the manager does not create husk pods for every
	// pool every other test makes. With the default now husk-on in
	// cmd/controller, each test is explicit about its mode so both paths stay
	// covered.
	_ = (&controller.SandboxPoolReconciler{
		Client:         mgr.GetClient(),
		NodeRegistry:   nodeRegistry,
		EnableHuskPods: false,
		KMS:            testKMS,
	}).SetupWithManager(mgr)

	rawClaim := &controller.SandboxReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		KMS:          testKMS,
		APIReader:    mgr.GetAPIReader(),
		NodeRegistry: nodeRegistry,
	}
	// The raw (forkd) reconciler ignores husk-test and husk-fork-test sandboxes so
	// it does not fight the husk reconcilers over the same object.
	rawClaim.SkipLabels(controller.HuskTestClaimLabel, controller.HuskForkTestLabel)
	// Wire the change feed: the buffered FakeRecorder for the always-on Event
	// mirror and the recording sink for the CloudEvents egress. A nil clock uses
	// the wall clock; the feed tests assert the envelope, not an exact time.
	rawClaim.SetFeedForTest(testEventRecorder, testSink, nil)
	// Route the memory-snapshot pairing seams through the per-test swappable
	// fakes (W4 Task 2). Safe defaults: checkpoint captures nothing, resume
	// errors (a test that wants resume must install it), exists reports absent.
	rawClaim.SetMemorySnapshotForTest(
		func(ctx context.Context, claim *v1.Sandbox) (controller.MemSnapshotResultForTest, error) {
			if fn := currentMemCheckpoint(); fn != nil {
				return fn(ctx, claim)
			}
			return controller.MemSnapshotResultForTest{}, nil
		},
		func(ctx context.Context, claim *v1.Sandbox, ref string) error {
			if fn := currentMemResume(); fn != nil {
				return fn(ctx, claim, ref)
			}
			return fmt.Errorf("no memory resume fake installed")
		},
		func(ctx context.Context, ref, principal string) (bool, error) {
			if fn := currentMemExists(); fn != nil {
				return fn(ctx, ref, principal)
			}
			return false, nil
		},
	)
	// Route the workspace hydrate/dehydrate seams through the per-test swappable
	// fakes so the binding lifecycle is driven without a VM. A test that does not
	// install fakes but uses a workspaceRef gets a fail-closed default.
	rawClaim.SetWorkspaceTransferForTest(
		func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error {
			if fn := currentWSHydrate(); fn != nil {
				return fn(ctx, claim, manifest)
			}
			return fmt.Errorf("no workspace hydrate fake installed")
		},
		func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error) {
			if fn := currentWSDehydrate(); fn != nil {
				return fn(ctx, claim, excludePaths, capturePaths)
			}
			return "", fmt.Errorf("no workspace dehydrate fake installed")
		},
		func(ctx context.Context, claim *v1.Sandbox, parent, child cas.Digest) (workspace.Diff, error) {
			if fn := currentWSDiff(); fn != nil {
				return fn(ctx, claim, parent, child)
			}
			return workspace.Diff{}, nil
		},
		func(ctx context.Context, repoFiles map[string]string, remote, branch string, creds *workspace.Credentials) error {
			if fn := currentWSRendezvous(); fn != nil {
				return fn(ctx, repoFiles, remote, branch, creds)
			}
			return nil
		},
		func(ctx context.Context, claim *v1.Sandbox, digest cas.Digest, gitPaths []string) (map[string]string, error) {
			if fn := currentWSRepoFiles(); fn != nil {
				return fn(ctx, claim, digest, gitPaths)
			}
			return nil, nil
		},
	)
	if err := rawClaim.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	// A husk-enabled claim reconciler that handles ONLY husk-test claims. Its
	// activator is swappable per test via setHuskTestActivator.
	huskClaim := &controller.SandboxReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		APIReader:         mgr.GetAPIReader(),
		NodeRegistry:      nodeRegistry,
		EnableHuskPods:    true,
		KMS:               testKMS,
		HuskTLS:           &tls.Config{}, //nolint:gosec // test stub; the fake activator ignores it
		HuskStubImage:     "mitos-husk-stub:test",
		DataDir:           "/var/lib/mitos",
		HuskTLSSecretName: controller.HuskTLSSecretName,
		HuskCASecretName:  controller.CASecretName,
	}
	// One husk reconciler handles BOTH the husk-claim (husk-test) and husk-fork
	// (husk-fork-test) sandboxes: the consolidated reconciler owns both engines, so
	// a husk pod event enqueues the sandbox on exactly one reconciler (no race).
	huskClaim.OnlyLabels(controller.HuskTestClaimLabel, controller.HuskForkTestLabel)
	huskClaim.SetActivateForTest(func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error) {
		// The activate seam is shared by the husk-claim path and the husk-fork child
		// path. A husk-fork test installs currentForkActivator; a husk-claim test
		// installs currentHuskTestActivator. They are set per-test and never both at
		// once, so prefer the fork activator when a fork test installed one.
		if forkActivatorInstalled() {
			return currentForkActivator()(ctx, addr, tlsConf, req)
		}
		return currentHuskTestActivator()(ctx, addr, tlsConf, req)
	})
	huskClaim.SetForkSnapshotForTest(func(c context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return currentForkSnapshotter()(c, addr, tlsConf, req)
	})
	huskClaim.SetForkSnapshotRemoverForTest(func(c context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error) {
		return currentForkSnapshotRemover()(c, addr, tlsConf, req)
	})
	// The MultiVMFork routing: the spawn-vm seam and a race-safe gate a per-case
	// test flips. The gate defaults off, so every existing fork test keeps the
	// byte-for-byte new-pod path.
	huskClaim.SetForkVMSpawnerForTest(func(c context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error) {
		return currentForkVMSpawner()(c, addr, tlsConf, req)
	})
	huskClaim.SetMultiVMForkGateForTest(currentForkMultiVM)
	huskClaim.SetCheckpointForTest(func(c context.Context, claim *v1.Sandbox, pod *corev1.Pod) (bool, error) {
		if fn := currentHuskTestCheckpointer(); fn != nil {
			return fn(c, claim, pod)
		}
		return false, nil
	})
	// Route the husk-mode workspace transport delegates through the per-test
	// swappable fakes so the husk reconciler's default hydrate/dehydrate path
	// (which delegates to the husk-stub control op in husk mode) is exercised
	// without a real husk pod. A test that uses a workspaceRef installs both via
	// setHuskWSDelegate.
	huskClaim.SetWorkspaceDelegateForTest(
		func(ctx context.Context, claim *v1.Sandbox, manifest cas.Digest) error {
			if fn := currentHuskWSHydrate(); fn != nil {
				return fn(ctx, claim, manifest)
			}
			return fmt.Errorf("no husk workspace hydrate delegate installed")
		},
		func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string) (cas.Digest, error) {
			if fn := currentHuskWSDehydrate(); fn != nil {
				return fn(ctx, claim, excludePaths, capturePaths)
			}
			return "", fmt.Errorf("no husk workspace dehydrate delegate installed")
		},
	)
	// The husk-mode combined dehydrate+diff delegate stands in for the husk-stub op
	// that captures the workspace into the node CAS and (when wantDiff) computes the
	// diff from the two node-CAS manifests. It routes the digest through the same
	// dehydrate recorder and, when a diff is requested, through the installed diff
	// fake. This proves the husk terminate path commits a revision WITH a diff
	// summary without ever touching the in-controller workspaceTransport seam.
	huskClaim.SetWorkspaceDehydrateDiffDelegateForTest(
		func(ctx context.Context, claim *v1.Sandbox, excludePaths, capturePaths []string, parentManifest cas.Digest, wantDiff bool) (cas.Digest, *workspace.Diff, error) {
			dehydrate := currentHuskWSDehydrate()
			if dehydrate == nil {
				return "", nil, fmt.Errorf("no husk workspace dehydrate delegate installed")
			}
			digest, err := dehydrate(ctx, claim, excludePaths, capturePaths)
			if err != nil {
				return "", nil, err
			}
			if !wantDiff {
				return digest, nil, nil
			}
			diffFn := currentHuskWSDiff()
			if diffFn == nil {
				return digest, nil, nil
			}
			d, derr := diffFn(ctx, claim, parentManifest, digest)
			if derr != nil {
				return "", nil, derr
			}
			return digest, &d, nil
		},
	)
	if err := huskClaim.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	// The consolidated raw reconciler (rawClaim) already owns the raw-forkd fork
	// engine (source.fromSandbox), so no separate raw fork reconciler is wired: a
	// raw fork Sandbox carries neither husk label and is handled by rawClaim. The
	// husk-fork engine is owned by the single huskClaim reconciler above (it has the
	// fork-snapshot/activate/remove seams wired), so a husk pod event enqueues each
	// sandbox on exactly one reconciler and the two no longer race.

	// The Workspace reconciler (W4): manages the revision DAG, retention,
	// lineage, and head/revisions/resumable status. Core, not behind any flag.
	wsReconciler := &controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
	}
	// The resumable status verifies a head's paired memory snapshot exists
	// (principal-bound) through the same swappable fake the resume path uses, so
	// a GC'd snapshot flips resumable false in the same test that drove the
	// checkpoint.
	wsReconciler.SetSnapshotExistsForTest(func(ctx context.Context, ref, principal string) (bool, error) {
		if fn := currentMemExists(); fn != nil {
			return fn(ctx, ref, principal)
		}
		return false, nil
	})
	if err := wsReconciler.SetupWithManager(mgr); err != nil {
		panic(err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(err)
		}
	}()

	// Wait for manager cache sync
	time.Sleep(1 * time.Second)

	exitCode := m.Run()

	cancel()
	testEnv.Stop()
	_ = exitCode
}
