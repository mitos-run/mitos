package agentcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tokenSecretSuffix is appended to a sandbox name to form the name of the
// Secret holding its sandbox API bearer token. It mirrors the controller's
// constant (internal/controller/token_secret.go).
const tokenSecretSuffix = "-sandbox-token"

// Scheme is the runtime scheme with the mitos v1 and core types registered,
// for building a controller-runtime client against a real cluster.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(v1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}

// ClusterBackend implements Backend over a Kubernetes cluster: it creates
// Sandboxes, reads the per-sandbox token Secret, and drives exec and file IO
// over the sandbox's HTTP endpoint with the bearer token. The token value is
// read into memory only for the duration of a request and is never logged; the
// underlying mcp.HTTPBackend redacts any echo of it from error strings.
type ClusterBackend struct {
	client     client.Client
	namespace  string
	httpClient *http.Client
	now        func() time.Time

	pollInterval time.Duration
	pollTimeout  time.Duration

	// readyHook / forkReadyHook are test seams: when set, they are invoked once
	// right after the sandbox or fork is created, simulating the controller
	// reconciling it to Ready. In production they are nil and the poll observes
	// the real controller.
	readyHook     func(ctx context.Context, name string)
	forkReadyHook func(ctx context.Context, name string, n int)
}

// NewClusterBackend builds a ClusterBackend against the cluster reachable by c,
// scoped to namespace. A nil httpClient uses http.DefaultClient.
func NewClusterBackend(c client.Client, namespace string, httpClient *http.Client) *ClusterBackend {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if namespace == "" {
		namespace = "default"
	}
	return &ClusterBackend{
		client:       c,
		namespace:    namespace,
		httpClient:   httpClient,
		now:          time.Now,
		pollInterval: 500 * time.Millisecond,
		pollTimeout:  60 * time.Second,
	}
}

// randName returns a short random suffix so generated sandbox names do not
// collide across concurrent callers.
func randName(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// Create creates a Sandbox referencing pool via source.poolRef, waits for it
// to reach the Ready phase (bounded by pollTimeout), and returns the sandbox
// name as the sandbox id.
func (b *ClusterBackend) Create(ctx context.Context, pool string) (string, error) {
	if pool == "" {
		return "", fmt.Errorf("create: a pool is required")
	}
	name := randName("sbx")
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: pool},
			},
		},
	}
	if err := b.client.Create(ctx, sandbox); err != nil {
		return "", fmt.Errorf("create sandbox: %w", err)
	}
	if b.readyHook != nil {
		b.readyHook(ctx, name)
	}
	if err := b.waitSandboxReady(ctx, name); err != nil {
		return "", err
	}
	return name, nil
}

// waitSandboxReady polls the sandbox until its phase is Ready (success),
// Failed (error), or the timeout elapses.
func (b *ClusterBackend) waitSandboxReady(ctx context.Context, name string) error {
	deadline := b.now().Add(b.pollTimeout)
	for {
		var sandbox v1.Sandbox
		if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &sandbox); err != nil {
			return fmt.Errorf("get sandbox %s: %w", name, err)
		}
		switch sandbox.Status.Phase {
		case v1.SandboxReady:
			if sandbox.Status.Endpoint != "" {
				return nil
			}
		case v1.SandboxFailed:
			return fmt.Errorf("sandbox %s failed", name)
		}
		if b.now().After(deadline) {
			return fmt.Errorf("sandbox %s not ready after %s", name, b.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

// sandboxHTTP builds an mcp.HTTPBackend for the named sandbox by reading its
// endpoint and token Secret. The token is held only for the lifetime of the
// returned backend's request; the redaction in mcp.HTTPBackend keeps it out of
// any error string.
func (b *ClusterBackend) sandboxHTTP(ctx context.Context, name string) (*mcp.HTTPBackend, error) {
	var sandbox v1.Sandbox
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &sandbox); err != nil {
		return nil, fmt.Errorf("get sandbox %s: %w", name, err)
	}
	endpoint := sandbox.Status.Endpoint

	var secret corev1.Secret
	token := ""
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name + tokenSecretSuffix}, &secret); err == nil {
		token = string(secret.Data["token"])
		if endpoint == "" {
			endpoint = string(secret.Data["endpoint"])
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read token secret for %s: %w", name, err)
	}

	if endpoint == "" {
		return nil, fmt.Errorf("sandbox %s has no endpoint yet", name)
	}
	return mcp.NewHTTPBackend("http://"+endpoint, token, b.httpClient), nil
}

// Exec runs command in the sandbox over its HTTP endpoint with the bearer token.
func (b *ClusterBackend) Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (ExecResult, error) {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return ExecResult{}, err
	}
	res, err := hb.Exec(ctx, sandboxID, command, timeoutSec)
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

// ReadFile reads path from the sandbox over its HTTP endpoint.
func (b *ClusterBackend) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	return hb.ReadFile(ctx, sandboxID, path)
}

// WriteFile writes content to path in the sandbox over its HTTP endpoint.
func (b *ClusterBackend) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	hb, err := b.sandboxHTTP(ctx, sandboxID)
	if err != nil {
		return err
	}
	return hb.WriteFile(ctx, sandboxID, path, content)
}

// Fork creates a Sandbox with source.fromSandbox set to sandboxID and
// spec.replicas set to n, waits for the children to be Ready (bounded), and
// returns the child sandbox names.
func (b *ClusterBackend) Fork(ctx context.Context, sandboxID string, n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	name := randName(sandboxID + "-fork")
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.namespace},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				FromSandbox: &v1.FromSandboxSource{Name: sandboxID},
			},
			Replicas: int32(n),
		},
	}
	if err := b.client.Create(ctx, sandbox); err != nil {
		return nil, fmt.Errorf("create fork: %w", err)
	}
	if b.forkReadyHook != nil {
		b.forkReadyHook(ctx, name, n)
	}
	return b.waitChildrenReady(ctx, name, n)
}

// waitChildrenReady polls the Sandbox until at least n children are Ready,
// then returns their names. Children are reported in Status.Children.
func (b *ClusterBackend) waitChildrenReady(ctx context.Context, name string, n int) ([]string, error) {
	deadline := b.now().Add(b.pollTimeout)
	for {
		var sandbox v1.Sandbox
		if err := b.client.Get(ctx, client.ObjectKey{Namespace: b.namespace, Name: name}, &sandbox); err != nil {
			return nil, fmt.Errorf("get sandbox %s: %w", name, err)
		}
		ready := make([]string, 0, n)
		for i := range sandbox.Status.Children {
			child := &sandbox.Status.Children[i]
			if child.Phase == v1.SandboxReady {
				ready = append(ready, child.Name)
			}
		}
		if len(ready) >= n {
			return ready[:n], nil
		}
		if b.now().After(deadline) {
			return nil, fmt.Errorf("fork %s not ready after %s", name, b.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.pollInterval):
		}
	}
}

// Terminate deletes the Sandbox, which the controller reaps.
func (b *ClusterBackend) Terminate(ctx context.Context, sandboxID string) error {
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: sandboxID, Namespace: b.namespace},
	}
	if err := b.client.Delete(ctx, sandbox); err != nil {
		return fmt.Errorf("delete sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// List returns the Sandboxes in namespace mapped to SandboxInfo. An empty
// namespace lists across all namespaces.
func (b *ClusterBackend) List(ctx context.Context, namespace string) ([]SandboxInfo, error) {
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	var sandboxes v1.SandboxList
	if err := b.client.List(ctx, &sandboxes, opts...); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	now := b.now()
	infos := make([]SandboxInfo, 0, len(sandboxes.Items))
	for i := range sandboxes.Items {
		s := &sandboxes.Items[i]
		pool := ""
		if s.Spec.Source.PoolRef != nil {
			pool = s.Spec.Source.PoolRef.Name
		}
		infos = append(infos, SandboxInfo{
			Name:     s.Name,
			Pool:     pool,
			Phase:    string(s.Status.Phase),
			Node:     s.Status.Node,
			Endpoint: s.Status.Endpoint,
			Age:      now.Sub(s.CreationTimestamp.Time),
		})
	}
	return infos, nil
}

// Workspace returns a ClusterWorkspaceBackend bound to the same client and
// namespace.
func (b *ClusterBackend) Workspace() WorkspaceBackend {
	return &ClusterWorkspaceBackend{
		client:       b.client,
		namespace:    b.namespace,
		now:          b.now,
		pollInterval: b.pollInterval,
		pollTimeout:  b.pollTimeout,
	}
}

// Template returns the template authoring surface over the cluster: it applies
// a SandboxPool with an inline template so the node-side snapshot build is
// triggered (KVM gated). Push is a publish marker for now.
func (b *ClusterBackend) Template() TemplateBackend {
	return &ClusterTemplateBackend{client: b.client, namespace: b.namespace}
}

// ClusterTemplateBackend creates or updates SandboxPool objects with an inline
// PoolTemplateSpec. The real snapshot build runs on a KVM node via forkd once
// the pool exists; this backend just authors the declarative object from the
// builder or a Dockerfile.
type ClusterTemplateBackend struct {
	client    client.Client
	namespace string
}

// Build creates or updates the named SandboxPool with an inline template spec.
func (t *ClusterTemplateBackend) Build(ctx context.Context, name string, spec v1.PoolTemplateSpec) error {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: t.namespace},
		Spec:       v1.SandboxPoolSpec{Template: &spec},
	}
	existing := &v1.SandboxPool{}
	err := t.client.Get(ctx, client.ObjectKey{Name: name, Namespace: t.namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := t.client.Create(ctx, pool); err != nil {
			return fmt.Errorf("create pool %s: %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("get pool %s: %w", name, err)
	default:
		existing.Spec = v1.SandboxPoolSpec{Template: &spec}
		if err := t.client.Update(ctx, existing); err != nil {
			return fmt.Errorf("update pool %s: %w", name, err)
		}
	}
	return nil
}

// Push is a no-op publish marker on the cluster backend: a pool applied to the
// cluster is already discoverable. It exists for CLI and Daytona parity and is
// a seam for a future registry push.
func (t *ClusterTemplateBackend) Push(_ context.Context, _ string) error {
	return nil
}

// ClusterWorkspaceBackend drives the workspace verbs over the cluster: it
// creates Workspace and WorkspaceRevision objects and reads their status. It
// reuses WorkspaceVerbs (the controller-side fork/revert helpers) so the
// lineage and rejection rules are shared with the controller.
type ClusterWorkspaceBackend struct {
	client    client.Client
	namespace string
	now       func() time.Time

	pollInterval time.Duration
	pollTimeout  time.Duration

	// readyHook is a test seam: when set, it is invoked once right after a
	// sandbox is created, simulating the controller reconciling it to Ready.
	// In production it is nil and the poll observes the real controller.
	readyHook func(ctx context.Context, name string)
}

func (w *ClusterWorkspaceBackend) verbs() *controller.WorkspaceVerbs {
	return &controller.WorkspaceVerbs{Client: w.client}
}

// CreateWorkspace creates an empty Workspace object.
func (w *ClusterWorkspaceBackend) CreateWorkspace(ctx context.Context, name string) error {
	ws := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.namespace}}
	if err := w.client.Create(ctx, ws); err != nil {
		return fmt.Errorf("create workspace %s: %w", name, err)
	}
	return nil
}

// ListWorkspaces lists the workspaces in namespace (or the backend default when
// namespace is empty), mapping them to WorkspaceInfo rows.
func (w *ClusterWorkspaceBackend) ListWorkspaces(ctx context.Context, namespace string) ([]WorkspaceInfo, error) {
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	} else {
		opts = append(opts, client.InNamespace(w.namespace))
	}
	var list v1.WorkspaceList
	if err := w.client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	now := w.now()
	out := make([]WorkspaceInfo, 0, len(list.Items))
	for i := range list.Items {
		ws := &list.Items[i]
		out = append(out, WorkspaceInfo{
			Name: ws.Name, Head: ws.Status.Head, Revisions: int(ws.Status.Revisions),
			Resumable: ws.Status.Resumable, Age: now.Sub(ws.CreationTimestamp.Time),
		})
	}
	return out, nil
}

// Log lists the revisions belonging to workspace, newest first.
func (w *ClusterWorkspaceBackend) Log(ctx context.Context, workspace string) ([]RevisionInfo, error) {
	var list v1.WorkspaceRevisionList
	if err := w.client.List(ctx, &list, client.InNamespace(w.namespace)); err != nil {
		return nil, fmt.Errorf("list revisions: %w", err)
	}
	now := w.now()
	out := make([]RevisionInfo, 0)
	for i := range list.Items {
		r := &list.Items[i]
		if r.Spec.WorkspaceRef.Name != workspace {
			continue
		}
		out = append(out, RevisionInfo{
			Name: r.Name, Phase: string(r.Status.Phase), Lineage: revisionLineageStr(r),
			Resumable: r.Spec.MemorySnapshotRef != nil, Age: now.Sub(r.CreationTimestamp.Time),
		})
	}
	// Newest first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Age < out[j].Age })
	return out, nil
}

// Diff returns the recorded content-hash diff of a revision against its parent
// head, if the revision captured one (via a terminate {diff: true} output).
func (w *ClusterWorkspaceBackend) Diff(ctx context.Context, workspace, revision string) (DiffInfo, error) {
	var rev v1.WorkspaceRevision
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: w.namespace, Name: revision}, &rev); err != nil {
		return DiffInfo{}, fmt.Errorf("get revision %s: %w", revision, err)
	}
	if rev.Status.DiffSummary == nil {
		return DiffInfo{}, fmt.Errorf("revision %s has no recorded diff; capture it with a terminate {diff:true} output", revision)
	}
	d := rev.Status.DiffSummary
	return DiffInfo{Parent: d.ParentRevision, Added: d.Added, Removed: d.Removed, Modified: d.Modified}, nil
}

// Fork branches a committed revision of src into dst, returning the new
// revision name. It delegates to the shared controller-side verb so the
// lineage and rejection rules match the controller.
func (w *ClusterWorkspaceBackend) Fork(ctx context.Context, src, rev, dst string) (string, error) {
	r, err := w.verbs().Fork(ctx, w.namespace, src, rev, dst)
	if err != nil {
		return "", err
	}
	return r.Name, nil
}

// Revert sets a workspace head to a past revision by creating a new tip that
// shares its content; returns the new revision name.
func (w *ClusterWorkspaceBackend) Revert(ctx context.Context, workspace, rev string) (string, error) {
	r, err := w.verbs().Revert(ctx, w.namespace, workspace, rev)
	if err != nil {
		return "", err
	}
	return r.Name, nil
}

// RemoveWorkspace deletes a workspace; its revisions are garbage-collected by
// owner reference.
func (w *ClusterWorkspaceBackend) RemoveWorkspace(ctx context.Context, name string) error {
	ws := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.namespace}}
	if err := w.client.Delete(ctx, ws); err != nil {
		return fmt.Errorf("delete workspace %s: %w", name, err)
	}
	return nil
}

// Bind binds a running sandbox to a workspace. A sandbox binds one workspace
// for its lifetime: re-binding to a different workspace is refused.
func (w *ClusterWorkspaceBackend) Bind(ctx context.Context, sandboxID, workspace string) error {
	var sandbox v1.Sandbox
	if err := w.client.Get(ctx, client.ObjectKey{Namespace: w.namespace, Name: sandboxID}, &sandbox); err != nil {
		return fmt.Errorf("get sandbox %s: %w", sandboxID, err)
	}
	if sandbox.Spec.WorkspaceRef != nil && sandbox.Spec.WorkspaceRef.Name != workspace {
		return fmt.Errorf("sandbox %s is already bound to workspace %s; a sandbox binds one workspace for its lifetime", sandboxID, sandbox.Spec.WorkspaceRef.Name)
	}
	patch := client.MergeFrom(sandbox.DeepCopy())
	sandbox.Spec.WorkspaceRef = &v1.LocalObjectReference{Name: workspace}
	if err := w.client.Patch(ctx, &sandbox, patch); err != nil {
		return fmt.Errorf("bind sandbox %s to workspace %s: %w", sandboxID, workspace, err)
	}
	return nil
}

// waitSandboxReady polls the sandbox until its phase is Ready (success),
// Failed (error), or the timeout elapses. It mirrors ClusterBackend.waitSandboxReady.
func (w *ClusterWorkspaceBackend) waitSandboxReady(ctx context.Context, name string) error {
	pollInterval := w.pollInterval
	if pollInterval == 0 {
		pollInterval = 500 * time.Millisecond
	}
	pollTimeout := w.pollTimeout
	if pollTimeout == 0 {
		pollTimeout = 60 * time.Second
	}
	deadline := w.now().Add(pollTimeout)
	for {
		var sandbox v1.Sandbox
		if err := w.client.Get(ctx, client.ObjectKey{Namespace: w.namespace, Name: name}, &sandbox); err != nil {
			return fmt.Errorf("get sandbox %s: %w", name, err)
		}
		switch sandbox.Status.Phase {
		case v1.SandboxReady:
			if sandbox.Status.Endpoint != "" {
				return nil
			}
		case v1.SandboxFailed:
			return fmt.Errorf("sandbox %s failed", name)
		}
		if w.now().After(deadline) {
			return fmt.Errorf("sandbox %s not ready after %s", name, pollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Serve creates a Sandbox with a workspaceRef and expose configuration, waits
// for it to be Ready, and returns a ServeResult with the expose URL. The URL
// is reachable once the expose proxy is deployed and *.<exposeDomain> DNS
// resolves to it.
//
// For sharing=="link": link-token minting is deferred to slice 5b; the clean
// URL is returned for now.
func (w *ClusterWorkspaceBackend) Serve(ctx context.Context, workspace, exposeDomain string, opts ServeOptions) (ServeResult, error) {
	if opts.Pool == "" {
		return ServeResult{}, fmt.Errorf("serve: pool is required")
	}
	if exposeDomain == "" {
		return ServeResult{}, fmt.Errorf("serve: expose domain is required")
	}

	name := randName("sbx")

	effectivePort := opts.Port
	if effectivePort == 0 {
		effectivePort = 8080
	}
	effectiveSharing := opts.Sharing
	if effectiveSharing == "" {
		effectiveSharing = "private"
	}
	effectiveLabel := opts.Label
	if effectiveLabel == "" {
		effectiveLabel = name
	}

	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.namespace},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				PoolRef: &v1.LocalObjectReference{Name: opts.Pool},
			},
			WorkspaceRef: &v1.LocalObjectReference{Name: workspace},
			Expose: &v1.SandboxExpose{
				Port:    int32(effectivePort),
				Label:   effectiveLabel,
				Sharing: effectiveSharing,
			},
		},
	}
	if err := w.client.Create(ctx, sandbox); err != nil {
		return ServeResult{}, fmt.Errorf("create sandbox for workspace serve: %w", err)
	}
	if w.readyHook != nil {
		w.readyHook(ctx, name)
	}
	if err := w.waitSandboxReady(ctx, name); err != nil {
		return ServeResult{}, err
	}

	url, err := BuildExposeURL(effectiveLabel, exposeDomain)
	if err != nil {
		return ServeResult{}, fmt.Errorf("serve: %w", err)
	}

	return ServeResult{
		SandboxName: name,
		Label:       effectiveLabel,
		URL:         url,
		Sharing:     effectiveSharing,
	}, nil
}

// revisionLineageStr renders the human-legible lineage of a revision.
func revisionLineageStr(r *v1.WorkspaceRevision) string {
	if r.Spec.Source.FromClaim != "" {
		return "fromClaim:" + r.Spec.Source.FromClaim
	}
	if r.Spec.Source.FromWorkspaceRevision != nil {
		return "fromWorkspaceRevision:" + r.Spec.Source.FromWorkspaceRevision.Revision
	}
	return "root"
}
