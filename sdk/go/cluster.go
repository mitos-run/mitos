package mitos

// Kubernetes cluster mode for the Go SDK: the AgentRun client drives the
// mitos.run/v1 CRDs (SandboxPool, Sandbox, Workspace) directly, the same
// surface the Python AgentRun (sdk/python/mitos/client.py) and the TypeScript
// AgentRun expose. It is the operator path: a Sandbox is born from a pool (or
// forked from another sandbox) and the controller drives it to Ready.
//
// Cluster mode is built on the minimal stdlib Kubernetes client in k8s.go; it
// pulls NO third-party dependency and leaves direct mode (SandboxServer)
// untouched.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

const defaultPoolPrefix = "mitos-default-"

// slugRe collapses any run of characters outside [a-z0-9.-] into a single "-".
// It mirrors the Python _SLUG_RE and the TypeScript slug regex byte-for-byte.
var slugRe = regexp.MustCompile(`[^a-z0-9.-]+`)

// DefaultPoolName derives the deterministic default-pool name for an image. The
// image is lowercased, "/" and ":" become "-", any other unsafe character
// collapses to "-", the slug is bounded to 40 characters, and leading/trailing
// "-" and "." are stripped (a trailing "." is an invalid object-name tail). The
// result is prefixed with "mitos-default-". It is kept byte-for-byte identical
// to the Python default_pool_name and the TypeScript defaultPoolName.
func DefaultPoolName(image string) string {
	slug := strings.ToLower(image)
	slug = strings.ReplaceAll(slug, "/", "-")
	slug = strings.ReplaceAll(slug, ":", "-")
	slug = slugRe.ReplaceAllString(slug, "-")
	// Bound first, then strip trailing/leading "-" and "." so truncation can
	// never leave a name ending in "." or "-" (both invalid object-name tails).
	if len(slug) > 40 {
		slug = slug[:40]
	}
	slug = strings.Trim(slug, "-.")
	return defaultPoolPrefix + slug
}

// SandboxPhase is a Sandbox lifecycle phase as reported in status.phase.
type SandboxPhase string

const (
	// PhasePending is the initial phase before the controller schedules and
	// activates the sandbox.
	PhasePending SandboxPhase = "Pending"
	// PhaseRestoring is the phase while the snapshot is being restored.
	PhaseRestoring SandboxPhase = "Restoring"
	// PhaseReady is the phase once the sandbox is serving its API.
	PhaseReady SandboxPhase = "Ready"
	// PhaseTerminating is the phase after a terminate is requested.
	PhaseTerminating SandboxPhase = "Terminating"
	// PhaseFailed is the terminal failure phase.
	PhaseFailed SandboxPhase = "Failed"
)

// PoolStatus is the observed status of a SandboxPool.
type PoolStatus struct {
	// Name is the pool name.
	Name string
	// ReadySnapshots is the number of warm snapshots ready to fork from.
	ReadySnapshots int
	// Desired is the pool's spec.replicas.
	Desired int
	// NodeDistribution maps node name to ready-snapshot count on that node.
	NodeDistribution map[string]int
}

// AgentRun is the Kubernetes cluster-mode client: it reconciles with the
// mitos.run/v1 CRDs directly. Construct it with NewAgentRun and the connection
// options (WithInCluster or WithKubeconfig, WithNamespace). It is the Go
// analogue of the Python AgentRun.
type AgentRun struct {
	k8s              *k8sClient
	namespace        string
	allowDefaultPool bool
}

// AgentRunOption configures NewAgentRun.
type AgentRunOption func(*agentRunConfig)

type agentRunConfig struct {
	namespace        string
	kubeconfig       string
	inCluster        bool
	allowDefaultPool bool
	cfg              *k8sConfig // injected directly in tests
}

// WithNamespace sets the namespace the AgentRun operates in. The default is
// "default".
func WithNamespace(namespace string) AgentRunOption {
	return func(c *agentRunConfig) { c.namespace = namespace }
}

// WithKubeconfig loads the connection from a kubeconfig file (the current
// context). An empty path falls back to $KUBECONFIG then $HOME/.kube/config. The
// SDK parses a common kubeconfig subset (server, CA, bearer token or client
// cert/key); it does not support exec credential plugins.
func WithKubeconfig(path string) AgentRunOption {
	return func(c *agentRunConfig) { c.kubeconfig = path; c.inCluster = false }
}

// WithInCluster loads the connection from the in-cluster service-account mount
// (KUBERNETES_SERVICE_HOST/PORT, the projected token, and the mounted CA). Use
// it when the SDK runs inside a Kubernetes pod.
func WithInCluster() AgentRunOption {
	return func(c *agentRunConfig) { c.inCluster = true }
}

// WithAllowDefaultPool toggles the lazy default-pool convenience. When false,
// Sandbox(image) refuses to create a pool and requires an explicit pool. The
// default is true.
func WithAllowDefaultPool(allow bool) AgentRunOption {
	return func(c *agentRunConfig) { c.allowDefaultPool = allow }
}

// withK8sConfig injects a resolved k8sConfig directly, bypassing kubeconfig and
// in-cluster loading. It is used by tests to point AgentRun at an httptest
// server. It is unexported so it is not part of the public surface.
func withK8sConfig(cfg *k8sConfig) AgentRunOption {
	return func(c *agentRunConfig) { c.cfg = cfg }
}

// NewAgentRun builds a cluster-mode client. With no connection option it loads
// the default kubeconfig ($KUBECONFIG, then $HOME/.kube/config). Pass
// WithInCluster() inside a pod or WithKubeconfig(path) for an explicit file.
func NewAgentRun(opts ...AgentRunOption) (*AgentRun, error) {
	cfg := agentRunConfig{namespace: "default", allowDefaultPool: true}
	for _, opt := range opts {
		opt(&cfg)
	}

	var conn *k8sConfig
	var err error
	switch {
	case cfg.cfg != nil:
		conn = cfg.cfg
	case cfg.inCluster:
		conn, err = loadInClusterConfig()
	default:
		conn, err = loadKubeconfig(cfg.kubeconfig)
	}
	if err != nil {
		return nil, err
	}

	return &AgentRun{
		k8s:              newK8sClient(conn),
		namespace:        cfg.namespace,
		allowDefaultPool: cfg.allowDefaultPool,
	}, nil
}

// Namespace is the namespace this client operates in.
func (a *AgentRun) Namespace() string { return a.namespace }

// SandboxOption configures Sandbox and Create.
type SandboxOption func(*sandboxConfig)

type sandboxConfig struct {
	pool      string
	name      string
	env       map[string]string
	secrets   map[string]SecretRef
	ttl       string
	workspace string
	replicas  int
}

// SecretRef references a key in a Kubernetes Secret to inject as an environment
// variable, mirroring the Python secrets={env: (secret, key)} mapping.
type SecretRef struct {
	// SecretName is the name of the Secret object.
	SecretName string
	// Key is the key within the Secret's data.
	Key string
}

// WithPool selects an existing SandboxPool to claim from. With WithPool set,
// Sandbox does not create any default pool.
func WithPool(pool string) SandboxOption {
	return func(c *sandboxConfig) { c.pool = pool }
}

// WithName sets an explicit sandbox name. When omitted a sandbox-<hex> name is
// generated.
func WithName(name string) SandboxOption {
	return func(c *sandboxConfig) { c.name = name }
}

// WithEnv injects environment variables into the sandbox (spec.env).
func WithEnv(env map[string]string) SandboxOption {
	return func(c *sandboxConfig) { c.env = env }
}

// WithSecrets injects Secret-backed environment variables (spec.secrets). The
// map is env-var name to the (SecretName, Key) it reads. Secret VALUES never
// pass through the SDK; only the references do.
func WithSecrets(secrets map[string]SecretRef) SandboxOption {
	return func(c *sandboxConfig) { c.secrets = secrets }
}

// WithTTL bounds the sandbox lifetime (spec.lifetime.ttl), for example "30m" or
// "1h".
func WithTTL(ttl string) SandboxOption {
	return func(c *sandboxConfig) { c.ttl = ttl }
}

// WithWorkspace binds the sandbox to a durable Workspace by name
// (spec.workspaceRef). On activation the controller hydrates the workspace head
// into /workspace; on terminate it dehydrates a new committed revision.
func WithWorkspace(name string) SandboxOption {
	return func(c *sandboxConfig) { c.workspace = name }
}

// WithReplicas sets spec.replicas on the created Sandbox.
func WithReplicas(n int) SandboxOption {
	return func(c *sandboxConfig) { c.replicas = n }
}

// Sandbox is the one-liner entry point. With WithPool it claims from that
// existing pool and creates nothing else. Otherwise it ensures the default pool
// for image exists (creating it with an inline template when absent and
// allowed), then creates a Sandbox from it. Exactly one of image or WithPool is
// required.
func (a *AgentRun) Sandbox(ctx context.Context, image string, opts ...SandboxOption) (*ClusterSandbox, error) {
	cfg := sandboxConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	pool := cfg.pool
	if pool == "" && image == "" {
		return nil, &Error{
			Code:        "missing_image_or_pool",
			Message:     "Sandbox needs an image or a pool",
			Cause:       "neither image nor WithPool was provided",
			Remediation: `Pass an image like "python" for a lazy default pool, or WithPool("my-pool") for an existing pool.`,
		}
	}
	if pool == "" {
		if !a.allowDefaultPool {
			return nil, &Error{
				Code:        "no_default_pool",
				Message:     "default pools are disabled on this client",
				Cause:       "WithAllowDefaultPool(false) was set",
				Remediation: `Pass WithPool(name) for an existing pool, or construct NewAgentRun without WithAllowDefaultPool(false).`,
			}
		}
		resolved, err := a.ensureDefaultPool(ctx, image)
		if err != nil {
			return nil, err
		}
		pool = resolved
	}

	createOpts := []SandboxOption{WithPool(pool)}
	if cfg.name != "" {
		createOpts = append(createOpts, WithName(cfg.name))
	}
	if cfg.env != nil {
		createOpts = append(createOpts, WithEnv(cfg.env))
	}
	if cfg.secrets != nil {
		createOpts = append(createOpts, WithSecrets(cfg.secrets))
	}
	if cfg.ttl != "" {
		createOpts = append(createOpts, WithTTL(cfg.ttl))
	}
	if cfg.workspace != "" {
		createOpts = append(createOpts, WithWorkspace(cfg.workspace))
	}
	if cfg.replicas != 0 {
		createOpts = append(createOpts, WithReplicas(cfg.replicas))
	}
	return a.Create(ctx, createOpts...)
}

// ensureDefaultPool get-or-creates the default SandboxPool for an image and
// returns its name. A pre-existing pool is reused untouched (its inline image is
// verified against the requested image to guard a slug collision); a missing one
// is created as a single SandboxPool with inline spec.template. A 409 from a
// concurrent creator is tolerated.
func (a *AgentRun) ensureDefaultPool(ctx context.Context, image string) (string, error) {
	name := DefaultPoolName(image)
	existing, err := a.k8s.getObject(ctx, a.namespace, "sandboxpools", name)
	if err == nil {
		if verr := verifyPoolImage(existing, name, image); verr != nil {
			return "", verr
		}
		return name, nil
	}
	if statusOf(err) != 404 {
		return "", err
	}

	pool := k8sObject{
		"apiVersion": k8sAPIGroup + "/" + k8sAPIVersion,
		"kind":       "SandboxPool",
		"metadata":   map[string]any{"name": name, "namespace": a.namespace},
		"spec": map[string]any{
			"template": map[string]any{"image": image},
			"replicas": 1,
		},
	}
	if _, err := a.k8s.createObject(ctx, a.namespace, "sandboxpools", pool); err != nil {
		if statusOf(err) == 409 { // raced another creator; reuse it
			return name, nil
		}
		return "", err
	}
	return name, nil
}

// verifyPoolImage guards the default-pool reuse path against a slug collision
// serving the wrong image. It reads the reused pool's inline spec.template.image
// and fails closed when it is absent or does not match the requested image.
func verifyPoolImage(pool k8sObject, name, image string) error {
	existingImage := nestedString(pool, "spec", "template", "image")
	if existingImage == "" {
		return &Error{
			Code:        "pool_image_mismatch",
			Message:     fmt.Sprintf("default pool %s has no readable inline template image", name),
			Cause:       fmt.Sprintf("pool %s spec.template.image is absent or unreadable", name),
			Remediation: fmt.Sprintf("Pass WithPool(%q) explicitly to reuse this pool, or use a distinct image that maps to a different default pool.", name),
		}
	}
	if existingImage != image {
		return &Error{
			Code:        "pool_image_mismatch",
			Message:     fmt.Sprintf("default pool %s already exists for a different image", name),
			Cause:       fmt.Sprintf("pool %s runs image %q, not the requested %q (the image slug collides)", name, existingImage, image),
			Remediation: fmt.Sprintf("Pass WithPool(%q) explicitly to reuse this pool, or use a distinct image that maps to a different default pool.", name),
		}
	}
	return nil
}

// Create creates a Sandbox from a pool (WithPool is required). When no name is
// given a sandbox-<hex> name is generated. env/secrets/ttl/workspace/replicas
// shape the spec. The returned handle resolves its endpoint and token lazily.
func (a *AgentRun) Create(ctx context.Context, opts ...SandboxOption) (*ClusterSandbox, error) {
	cfg := sandboxConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.pool == "" {
		return nil, &Error{
			Code:        "missing_pool",
			Message:     "Create needs a pool",
			Cause:       "WithPool was not provided",
			Remediation: `Pass WithPool(name); or use Sandbox(image) for the lazy default-pool path.`,
		}
	}
	name := cfg.name
	if name == "" {
		name = "sandbox-" + randomHex(4)
	}

	spec := map[string]any{
		"source": map[string]any{"poolRef": map[string]any{"name": cfg.pool}},
	}
	if cfg.replicas != 0 {
		spec["replicas"] = cfg.replicas
	}
	if len(cfg.env) > 0 {
		spec["env"] = envList(cfg.env)
	}
	if len(cfg.secrets) > 0 {
		secrets := make([]map[string]any, 0, len(cfg.secrets))
		for envVar, ref := range cfg.secrets {
			secrets = append(secrets, map[string]any{
				"name":      envVar,
				"secretRef": map[string]any{"name": ref.SecretName, "key": ref.Key},
				"envVar":    envVar,
			})
		}
		spec["secrets"] = secrets
	}
	if cfg.ttl != "" {
		spec["lifetime"] = map[string]any{"ttl": cfg.ttl}
	}
	if cfg.workspace != "" {
		spec["workspaceRef"] = map[string]any{"name": cfg.workspace}
	}

	body := k8sObject{
		"apiVersion": k8sAPIGroup + "/" + k8sAPIVersion,
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": name, "namespace": a.namespace},
		"spec":       spec,
	}
	if _, err := a.k8s.createObject(ctx, a.namespace, "sandboxes", body); err != nil {
		return nil, err
	}
	return &ClusterSandbox{
		Name:      name,
		Namespace: a.namespace,
		Pool:      cfg.pool,
		agent:     a,
	}, nil
}

// envList renders an env map as the spec.env list of {name,value} entries.
func envList(env map[string]string) []map[string]any {
	out := make([]map[string]any, 0, len(env))
	for k, v := range env {
		out = append(out, map[string]any{"name": k, "value": v})
	}
	return out
}

// FromName reconnects to an existing sandbox by name, returning a live handle
// resolved from the cluster. It is an alias for Get, named for the reconnect use
// case.
func (a *AgentRun) FromName(ctx context.Context, name string) (*ClusterSandbox, error) {
	return a.Get(ctx, name)
}

// Get reads an existing sandbox by name and returns a handle carrying its
// resolved pool, phase, and endpoint. When the sandbox is Ready its per-sandbox
// bearer token is loaded from the <name>-sandbox-token Secret.
func (a *AgentRun) Get(ctx context.Context, name string) (*ClusterSandbox, error) {
	obj, err := a.k8s.getObject(ctx, a.namespace, "sandboxes", name)
	if err != nil {
		return nil, err
	}
	pool := nestedString(obj, "spec", "source", "poolRef", "name")
	sb := &ClusterSandbox{
		Name:      name,
		Namespace: a.namespace,
		Pool:      pool,
		Phase:     SandboxPhase(statusPhase(obj)),
		Endpoint:  nestedString(obj, "status", "endpoint"),
		agent:     a,
	}
	if sb.Phase == PhaseReady {
		sb.loadToken(ctx)
	}
	return sb, nil
}

// List lists sandboxes in the namespace, optionally filtered by pool. An empty
// pool returns every sandbox.
func (a *AgentRun) List(ctx context.Context, pool string) ([]*ClusterSandbox, error) {
	list, err := a.k8s.listObjects(ctx, a.namespace, "sandboxes")
	if err != nil {
		return nil, err
	}
	var out []*ClusterSandbox
	for _, obj := range list.Items {
		objPool := nestedString(obj, "spec", "source", "poolRef", "name")
		if pool != "" && objPool != pool {
			continue
		}
		out = append(out, &ClusterSandbox{
			Name:      nestedString(obj, "metadata", "name"),
			Namespace: a.namespace,
			Pool:      objPool,
			Phase:     SandboxPhase(statusPhase(obj)),
			Endpoint:  nestedString(obj, "status", "endpoint"),
			agent:     a,
		})
	}
	return out, nil
}

// PoolStatus reads the status of a SandboxPool.
func (a *AgentRun) PoolStatus(ctx context.Context, name string) (*PoolStatus, error) {
	obj, err := a.k8s.getObject(ctx, a.namespace, "sandboxpools", name)
	if err != nil {
		return nil, err
	}
	dist := map[string]int{}
	if raw, ok := nestedMap(obj, "status", "nodeDistribution"); ok {
		for node, v := range raw {
			dist[node] = toInt(v)
		}
	}
	return &PoolStatus{
		Name:             name,
		ReadySnapshots:   toInt(nestedValue(obj, "status", "readySnapshots")),
		Desired:          toInt(nestedValue(obj, "spec", "replicas")),
		NodeDistribution: dist,
	}, nil
}

// CreateWorkspace creates an empty durable Workspace and returns a handle.
func (a *AgentRun) CreateWorkspace(ctx context.Context, name string) (*Workspace, error) {
	body := k8sObject{
		"apiVersion": k8sAPIGroup + "/" + k8sAPIVersion,
		"kind":       "Workspace",
		"metadata":   map[string]any{"name": name, "namespace": a.namespace},
		"spec":       map[string]any{},
	}
	if _, err := a.k8s.createObject(ctx, a.namespace, "workspaces", body); err != nil {
		return nil, err
	}
	return &Workspace{Name: name, Namespace: a.namespace, agent: a}, nil
}

// Workspace returns a lazy handle to a workspace by name. It does not touch the
// cluster until a verb is called; use CreateWorkspace to create one or
// GetWorkspace to reconnect and verify it exists.
func (a *AgentRun) Workspace(name string) *Workspace {
	return &Workspace{Name: name, Namespace: a.namespace, agent: a}
}

// GetWorkspace reconnects to an existing workspace, returning an error when it
// is absent.
func (a *AgentRun) GetWorkspace(ctx context.Context, name string) (*Workspace, error) {
	ws := &Workspace{Name: name, Namespace: a.namespace, agent: a}
	if _, err := ws.get(ctx); err != nil {
		return nil, err
	}
	return ws, nil
}

// ListWorkspaces lists the workspaces in the client's namespace.
func (a *AgentRun) ListWorkspaces(ctx context.Context) ([]*Workspace, error) {
	list, err := a.k8s.listObjects(ctx, a.namespace, "workspaces")
	if err != nil {
		return nil, err
	}
	out := make([]*Workspace, 0, len(list.Items))
	for _, obj := range list.Items {
		out = append(out, &Workspace{
			Name:      nestedString(obj, "metadata", "name"),
			Namespace: a.namespace,
			agent:     a,
		})
	}
	return out, nil
}

// statusPhase reads status.phase, defaulting to Pending when absent.
func statusPhase(obj k8sObject) string {
	phase := nestedString(obj, "status", "phase")
	if phase == "" {
		return string(PhasePending)
	}
	return phase
}

// nestedValue walks obj along the given keys and returns the value at the path,
// or nil when any segment is absent or not a map.
func nestedValue(obj k8sObject, keys ...string) any {
	var cur any = map[string]any(obj)
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[k]
		if !ok {
			return nil
		}
	}
	return cur
}

// nestedString reads a string at the path, or "" when absent or not a string.
func nestedString(obj k8sObject, keys ...string) string {
	if s, ok := nestedValue(obj, keys...).(string); ok {
		return s
	}
	return ""
}

// nestedMap reads a map[string]any at the path.
func nestedMap(obj k8sObject, keys ...string) (map[string]any, bool) {
	m, ok := nestedValue(obj, keys...).(map[string]any)
	return m, ok
}

// toInt coerces a JSON-decoded number (float64, json.Number, or int) to an int,
// returning 0 for anything else.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// ClusterSandbox is a cluster-mode Sandbox handle (a CRD-backed sandbox), as
// opposed to the direct-mode *Sandbox. It carries the cluster identity (name,
// namespace, pool) and the last-observed phase and endpoint.
type ClusterSandbox struct {
	// Name is the Sandbox object name.
	Name string
	// Namespace is the Sandbox object namespace.
	Namespace string
	// Pool is the SandboxPool this sandbox was claimed from.
	Pool string
	// Phase is the last-observed lifecycle phase.
	Phase SandboxPhase
	// Endpoint is the last-observed serving endpoint (host:port), empty until
	// the sandbox is Ready.
	Endpoint string

	agent *AgentRun
	token string // per-sandbox bearer token; held in memory only, never logged
}

// loadToken reads the per-sandbox bearer token from the <name>-sandbox-token
// Secret. A missing Secret is tolerated (the sandbox stays tokenless). The token
// VALUE is held in memory only and is never logged.
func (s *ClusterSandbox) loadToken(ctx context.Context) {
	data, err := s.agent.k8s.readSecret(ctx, s.Namespace, s.Name+"-sandbox-token")
	if err != nil {
		return
	}
	if tok, ok := data["token"]; ok {
		s.token = tok
	}
}

// Token returns the per-sandbox bearer token if one has been loaded, for callers
// that drive the sandbox HTTP API themselves. It is empty when the sandbox is
// not Ready or has no token Secret.
func (s *ClusterSandbox) Token() string { return s.token }

// Terminate deletes the Sandbox object. The controller drives teardown (and, for
// a workspace-bound sandbox, the dehydrate on the way out).
func (s *ClusterSandbox) Terminate(ctx context.Context) error {
	return s.agent.k8s.deleteObject(ctx, s.Namespace, "sandboxes", s.Name)
}

// RevisionInfo is one Workspace revision in the log.
type RevisionInfo struct {
	Name      string
	Phase     string
	Lineage   string
	Resumable bool
	Created   string
}

// Workspace is a durable, forkable workspace handle. It is lazy: it does not
// touch the cluster until a verb is called. The verbs mirror the Python
// Workspace (git-shaped: Log, Head, Resumable).
type Workspace struct {
	// Name is the Workspace object name.
	Name string
	// Namespace is the Workspace object namespace.
	Namespace string

	agent *AgentRun
}

// get reads the Workspace object, mapping a 404 to a typed workspace_not_found
// error.
func (w *Workspace) get(ctx context.Context) (k8sObject, error) {
	obj, err := w.agent.k8s.getObject(ctx, w.Namespace, "workspaces", w.Name)
	if err != nil {
		if statusOf(err) == 404 {
			return nil, &Error{
				Code:        "workspace_not_found",
				Message:     fmt.Sprintf("workspace %s not found", w.Name),
				Cause:       err.Error(),
				Status:      404,
				Remediation: "Create it with AgentRun.CreateWorkspace(name) first.",
			}
		}
		return nil, err
	}
	return obj, nil
}

// Head returns the workspace head revision name (status.head).
func (w *Workspace) Head(ctx context.Context) (string, error) {
	obj, err := w.get(ctx)
	if err != nil {
		return "", err
	}
	return nestedString(obj, "status", "head"), nil
}

// Resumable reports whether the workspace head is resumable (status.resumable).
func (w *Workspace) Resumable(ctx context.Context) (bool, error) {
	obj, err := w.get(ctx)
	if err != nil {
		return false, err
	}
	if b, ok := nestedValue(obj, "status", "resumable").(bool); ok {
		return b, nil
	}
	return false, nil
}

// Log lists the workspace's revisions, newest first.
func (w *Workspace) Log(ctx context.Context) ([]RevisionInfo, error) {
	list, err := w.agent.k8s.listObjects(ctx, w.Namespace, "workspacerevisions")
	if err != nil {
		return nil, err
	}
	var revs []RevisionInfo
	for _, obj := range list.Items {
		if nestedString(obj, "spec", "workspaceRef", "name") != w.Name {
			continue
		}
		_, hasSnap := nestedValue(obj, "spec", "memorySnapshotRef").(map[string]any)
		revs = append(revs, RevisionInfo{
			Name:      nestedString(obj, "metadata", "name"),
			Phase:     nestedString(obj, "status", "phase"),
			Lineage:   lineage(obj),
			Resumable: hasSnap,
			Created:   nestedString(obj, "metadata", "creationTimestamp"),
		})
	}
	// Newest first by creation timestamp (RFC3339 strings sort lexically).
	for i := 0; i < len(revs); i++ {
		for j := i + 1; j < len(revs); j++ {
			if revs[j].Created > revs[i].Created {
				revs[i], revs[j] = revs[j], revs[i]
			}
		}
	}
	return revs, nil
}

// lineage describes a revision's source, mirroring the Python _lineage.
func lineage(obj k8sObject) string {
	if v := nestedString(obj, "spec", "source", "fromClaim"); v != "" {
		return "fromClaim:" + v
	}
	if m, ok := nestedMap(obj, "spec", "source", "fromWorkspaceRevision"); ok {
		rev, _ := m["revision"].(string)
		return "fromWorkspaceRevision:" + rev
	}
	return "root"
}
