// Package clustersandbox is the real console.SandboxControl: it queries the
// controller's v1 Sandbox records scoped to one org, the cluster-backed
// implementation of the live-sandbox seam (issue #2). Under hard isolation each
// org's sandboxes live in its own namespace (tenant.NamespaceForOrg), so org
// scoping is the namespace boundary plus the org label as defense in depth: a
// cross-org id is reported as not-found, indistinguishable from a missing one.
package clustersandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/mcp"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// tokenSecretSuffix is appended to a sandbox name to form the name of the
// Secret carrying its sandbox API bearer token. It mirrors the controller's
// own constant (internal/controller/token_secret.go) and agentcli's
// ClusterBackend (internal/agentcli/clusterbackend.go): three independent
// readers of the SAME Secret convention. A shared package would remove this
// duplication; it is a small, stable, well-commented constant and pulling
// either console or agentcli into the other's import graph is a bigger change
// than this workstream's scope.
const tokenSecretSuffix = "-sandbox-token"

// requestedVCPUsAnnotation / requestedMemGiBAnnotation record what a
// console-created sandbox's vcpu/mem selects asked for. See Control.Create's
// doc for why these are informational only.
const (
	requestedVCPUsAnnotation  = "mitos.run/requested-vcpus"
	requestedMemGiBAnnotation = "mitos.run/requested-mem-gib"
)

// Control implements console.SandboxControl against the Kubernetes API.
type Control struct {
	c          client.Client
	httpClient *http.Client
}

// New builds the cluster-backed sandbox control. Exec dials the sandbox's own
// HTTP endpoint directly (not through the Kubernetes API), so a nil
// http.Client defaults to http.DefaultClient.
func New(c client.Client) *Control {
	return &Control{c: c, httpClient: http.DefaultClient}
}

// randomSandboxName returns prefix plus a random hex suffix, so concurrent
// Create/Fork calls never collide on name. Mirrors agentcli.randName
// (internal/agentcli/clusterbackend.go); duplicated rather than imported for
// the same reason as tokenSecretSuffix above.
func randomSandboxName(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// List returns the org's sandboxes from its namespace, filtered by the org
// label.
func (s *Control) List(ctx context.Context, orgID string) ([]console.SandboxView, error) {
	var list v1.SandboxList
	if err := s.c.List(ctx, &list,
		client.InNamespace(tenant.NamespaceForOrg(orgID)),
		client.MatchingLabels(tenant.OrgLabels(orgID)),
	); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	out := make([]console.SandboxView, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, viewOf(&list.Items[i], orgID))
	}
	return out, nil
}

// Get returns one of the org's sandboxes by name. A sandbox in another org's
// namespace (or missing, or not carrying the org label) is console.ErrNotFound.
func (s *Control) Get(ctx context.Context, orgID, sandboxID string) (console.SandboxView, error) {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return console.SandboxView{}, err
	}
	return viewOf(sb, orgID), nil
}

// Terminate deletes one of the org's sandboxes. A cross-org or missing id is
// console.ErrNotFound and nothing is deleted.
func (s *Control) Terminate(ctx context.Context, orgID, sandboxID string) error {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return err
	}
	if err := s.c.Delete(ctx, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return console.ErrNotFound
		}
		return fmt.Errorf("delete sandbox: %w", err)
	}
	return nil
}

// get fetches the org's sandbox by name from its namespace and verifies the org
// label, collapsing missing / cross-org / mislabeled into ErrNotFound.
func (s *Control) get(ctx context.Context, orgID, sandboxID string) (*v1.Sandbox, error) {
	var sb v1.Sandbox
	key := client.ObjectKey{Namespace: tenant.NamespaceForOrg(orgID), Name: sandboxID}
	if err := s.c.Get(ctx, key, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, console.ErrNotFound
		}
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	if sb.Labels[tenant.OrgLabelKey] != orgID {
		return nil, console.ErrNotFound // present but not labelled for this org
	}
	return &sb, nil
}

// Create provisions a new Sandbox in org's namespace from req.Template
// (source.poolRef) and returns its view.
//
// IMPORTANT: v1.SandboxSpec has NO per-sandbox resource override; a
// sandbox's vcpu/mem sizing is entirely the pool template's own configured
// resources (api/v1/types.go SandboxResources lives on PoolTemplateSpec, not
// SandboxSpec). So req.VCPUs/MemGiB cannot be applied to the created
// sandbox's actual resources here; they are recorded as annotations purely
// for display (viewOf reads them back), NOT as an enforced request. This is a
// genuine, documented control-plane gap (making per-request sizing real would
// need either a per-sandbox override field on the CRD or a catalog of
// per-size pool templates), not a shortcut: the console honestly shows what
// was asked for without claiming it was granted.
func (s *Control) Create(ctx context.Context, orgID string, req console.CreateSandboxRequest) (console.SandboxView, error) {
	if req.Template == "" {
		return console.SandboxView{}, fmt.Errorf("create sandbox: a template is required")
	}
	labels := tenant.OrgLabels(orgID)
	// Region is stamped ONLY when the request named one (issue #712 phase 0):
	// an empty request.Region means the org's home region, itself resolved by
	// the deployment default, so no label is written rather than an
	// empty-string value. This sandbox is a tree root; every fork of it MUST
	// inherit this label verbatim (see Fork below), never re-resolve it.
	if req.Region != "" {
		labels[tenant.RegionLabelKey] = req.Region
	}
	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      randomSandboxName("sbx"),
			Namespace: tenant.NamespaceForOrg(orgID),
			Labels:    labels,
			Annotations: map[string]string{
				requestedVCPUsAnnotation:  strconv.Itoa(int(req.VCPUs)),
				requestedMemGiBAnnotation: strconv.Itoa(int(req.MemGiB)),
			},
		},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: req.Template}},
		},
	}
	if err := s.c.Create(ctx, sandbox); err != nil {
		return console.SandboxView{}, fmt.Errorf("create sandbox: %w", err)
	}
	return viewOf(sandbox, orgID), nil
}

// Fork creates count new top-level Sandbox objects, each with
// source.fromSandbox set to sandboxID (replicas defaults to 1), and returns
// their names. This deliberately differs from agentcli.ClusterBackend.Fork
// (one Sandbox with spec.replicas=N, whose N children are pod-level entries
// in status.children, never independently addressable CRDs): the console
// needs every fork to be Get/Terminate/Exec-able through this SAME
// SandboxControl and visible as its own node in the fork tree
// (clusterforktree walks v1.SandboxList; a replicas>1 Sandbox is ONE node
// there, not N). N separate Sandbox objects satisfy both.
//
// On a partial failure (e.g. the i-th create errors) the sandboxes created so
// far are left in place, not rolled back: the Kubernetes API has no
// multi-object transaction, and deleting them back out on error risks
// compounding one failure into two. The caller sees an error; the survivors
// still appear on the next List/ForkTree read.
func (s *Control) Fork(ctx context.Context, orgID, sandboxID string, count int) ([]string, error) {
	source, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return nil, err
	}
	// A fork inherits the source's region label VERBATIM, never re-resolved:
	// a live CoW fork cannot cross clusters (issue #712 phase 0), so region
	// is a property of the whole fork tree, fixed at the tree root's
	// creation, not a choice each fork makes independently. A source with no
	// region label (predates this field, or the deployment never stamped
	// one) simply propagates that absence.
	region := source.Labels[tenant.RegionLabelKey]
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		// A fresh label map per child: sharing one map instance across
		// multiple ObjectMeta literals would alias every child's labels to
		// the same backing map.
		labels := tenant.OrgLabels(orgID)
		if region != "" {
			labels[tenant.RegionLabelKey] = region
		}
		child := &v1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      randomSandboxName(sandboxID + "-fork"),
				Namespace: tenant.NamespaceForOrg(orgID),
				Labels:    labels,
			},
			Spec: v1.SandboxSpec{
				Source: v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: sandboxID}},
			},
		}
		if err := s.c.Create(ctx, child); err != nil {
			return ids, fmt.Errorf("create fork %d/%d of %s: %w", i+1, count, sandboxID, err)
		}
		ids = append(ids, child.Name)
	}
	return ids, nil
}

// Exec runs cmd inside the org's sandbox over its own HTTP endpoint (the same
// transport the CLI's ClusterBackend uses), reading the endpoint and bearer
// token from the sandbox's status and its token Secret. Returns ErrNotFound
// if sandboxID does not exist or belongs to a different org (checked via get,
// BEFORE the Secret or endpoint is read, so a cross-org id never reaches the
// sandbox's own token).
func (s *Control) Exec(ctx context.Context, orgID, sandboxID, cmd string, timeoutSec int) (console.ExecResult, error) {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return console.ExecResult{}, err
	}
	endpoint := sb.Status.Endpoint
	var secret corev1.Secret
	token := ""
	secretKey := client.ObjectKey{Namespace: sb.Namespace, Name: sandboxID + tokenSecretSuffix}
	if err := s.c.Get(ctx, secretKey, &secret); err == nil {
		token = string(secret.Data["token"])
		if endpoint == "" {
			endpoint = string(secret.Data["endpoint"])
		}
	} else if !apierrors.IsNotFound(err) {
		return console.ExecResult{}, fmt.Errorf("read token secret for %s: %w", sandboxID, err)
	}
	if endpoint == "" {
		return console.ExecResult{}, fmt.Errorf("sandbox %s has no endpoint yet", sandboxID)
	}
	hb := mcp.NewHTTPBackend("http://"+endpoint, token, s.httpClient)
	res, err := hb.Exec(ctx, sandboxID, cmd, timeoutSec)
	if err != nil {
		return console.ExecResult{}, err
	}
	return console.ExecResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// viewOf maps a v1.Sandbox to the console view. Template is the pool the
// sandbox started from; the engine id and node-bearing pod come from status.
// VCPUs/MemBytes are read back from the requested-size annotations Create
// writes (best-effort, informational only; see Create's doc) and are zero for
// any sandbox not created through this console (e.g. via the CLI or kubectl).
func viewOf(sb *v1.Sandbox, orgID string) console.SandboxView {
	template := ""
	if sb.Spec.Source.PoolRef != nil {
		template = sb.Spec.Source.PoolRef.Name
	}
	var vcpus int32
	var memBytes int64
	// ParseInt with bitSize 32, not Atoi + int32(v): Atoi returns a platform
	// int (64-bit on every real target), so a corrupted or hand-edited
	// annotation holding a value outside the int32 range would silently
	// truncate/wrap (CodeQL go/incorrect-integer-conversion) instead of being
	// rejected. ParseInt(..., 32) fails for any out-of-range value, so the
	// annotation is ignored (fields stay zero) exactly like a parse failure,
	// never wrapped into a bogus (possibly negative) size. Real annotations are
	// written by Create from console.SandboxCreateRequest.VCPUs/MemGiB, both
	// already int32 and bounds-checked against allowedVCPUs/allowedMemGiB, so
	// this only matters for out-of-band edits.
	// The v > 0 guard rejects a negative annotation (e.g. a hand-edited or
	// corrupted "-1") the same way an out-of-range or non-numeric one is
	// already rejected: fields stay zero rather than reporting a negative
	// VCPUs/MemBytes to a caller.
	if v, err := strconv.ParseInt(sb.Annotations[requestedVCPUsAnnotation], 10, 32); err == nil && v > 0 {
		vcpus = int32(v)
	}
	// memBytes = requested GiB << 30: bounding the parsed value to int32 here
	// also bounds the shift result well within int64 (max ~2^31 << 30 ==
	// 2^61), so it cannot overflow the way an unbounded int64 GiB value would.
	if v, err := strconv.ParseInt(sb.Annotations[requestedMemGiBAnnotation], 10, 32); err == nil && v > 0 {
		memBytes = int64(v) << 30
	}
	return console.SandboxView{
		ID:        sb.Name,
		OrgID:     orgID,
		Template:  template,
		Node:      sb.Status.Pod,
		Phase:     string(sb.Status.Phase),
		VCPUs:     vcpus,
		MemBytes:  memBytes,
		CreatedAt: sb.CreationTimestamp.Time,
		Region:    sb.Labels[tenant.RegionLabelKey],
	}
}
