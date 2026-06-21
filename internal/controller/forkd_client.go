package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	forkdpb "mitos.run/mitos/proto/forkd"
)

// isNotFound reports whether err (possibly wrapped) carries gRPC NotFound.
func isNotFound(err error) bool {
	return hasGRPCCode(err, codes.NotFound)
}

// isRetryableCapacity reports whether err (possibly wrapped) carries a forkd
// Fork rejection that should re-pend the claim rather than fail it terminally:
// ResourceExhausted (the node hit its MaxSandboxes count cap, PR #110, a
// schedule-time race) or Unavailable (the node went away mid-fork). Both are
// transient from the claim's view: another node, or this one once it drains,
// can take the fork, so the claim routes to the bounded NoCapacity re-pend.
func isRetryableCapacity(err error) bool {
	return hasGRPCCode(err, codes.ResourceExhausted) || hasGRPCCode(err, codes.Unavailable)
}

// hasGRPCCode reports whether err (possibly wrapped) carries the given gRPC
// status code anywhere in its unwrap chain.
func hasGRPCCode(err error, code codes.Code) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := status.FromError(e); ok && s.Code() == code {
			return true
		}
	}
	return false
}

// sandboxInfo queries the forkd on nodeName via ListSandboxes and returns the
// raw SandboxInfo for sandboxID, carrying the full work-aware lifecycle signals
// (last-activity, live set_timeout deadline, open-stream count, paused flag).
// ok is false when the node is unreachable or the sandbox is not in the
// listing. The dial and RPC are bounded by a short timeout so a slow or dead
// forkd cannot block the reconcile.
func sandboxInfo(ctx context.Context, registry *NodeRegistry, nodeName, sandboxID string) (*forkdpb.SandboxInfo, bool) {
	conn, err := registry.GetConnection(nodeName)
	if err != nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).ListSandboxes(cctx, &forkdpb.ListSandboxesRequest{})
	if err != nil {
		return nil, false
	}
	for _, s := range resp.Sandboxes {
		if s.SandboxId == sandboxID {
			return s, true
		}
	}
	return nil, false
}

// forkOnNode asks the forkd on the given node to fork a sandbox from a snapshot.
// The returned endpoint is the node's HTTP sandbox API: what clients (SDKs)
// actually talk to. apiToken is the bearer token forkd registers for the
// sandbox's HTTP API; it is never logged. network is the template's egress
// policy (egress mode + allowlist); nil leaves the ForkRequest's NetworkConfig
// unset and forkd applies no per-fork egress ruleset. The policy and allowlist
// entries (IPs/ports/names) are safe to log.
// labels carries the claim/pool/workspace/namespace identity the controller
// attaches to the fork so the node can LABEL the sandbox's Layer 3 guest
// telemetry (issue #164). The fields are Kubernetes object names, never secrets;
// a nil value (no claim context) leaves the sandbox's vitals unlabeled.
func (r *SandboxClaimReconciler) forkOnNode(ctx context.Context, node *NodeInfo, snapshotID, sandboxID string, env, secrets map[string]string, network *v1alpha1.NetworkPolicy, volumes []*forkdpb.VolumeMount, apiToken string, wrappedDEK []byte, kekID string, labels *forkdpb.VitalsLabels) (*forkResult, error) {
	// controller.forkOnNode is the child span whose context the otelgrpc client
	// handler injects over the wire so the forkd.Fork span joins this trace.
	// Only node and snapshot (config, no secrets) are recorded.
	ctx, span := tracer.Start(ctx, "controller.forkOnNode", trace.WithAttributes(
		attribute.String("node", node.Name),
		attribute.String("snapshot", snapshotID),
	))
	defer span.End()

	// Fail closed: an encrypted template's WRAPPED DEK travels in this request, so
	// the node connection must be mTLS. Refuse to send it over an insecure channel
	// (node.TLS nil and registry.TLS nil, i.e. PKI bootstrap disabled). A
	// plaintext template carries no DEK and is unaffected.
	if len(wrappedDEK) > 0 && !r.NodeRegistry.NodeMTLS(node.Name) {
		err := fmt.Errorf("refusing to deliver the wrapped DEK over an insecure gRPC channel to node %s: enable PKI bootstrap on the controller and mTLS on forkd, or disable template encryption", node.Name)
		span.RecordError(err)
		return nil, err
	}

	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).Fork(ctx, &forkdpb.ForkRequest{
		SnapshotId: snapshotID,
		SandboxId:  sandboxID,
		Env:        toEnvVars(env),
		Secrets:    toSecretVars(secrets),
		Network:    toNetworkConfig(network),
		Volumes:    volumes,
		ApiToken:   apiToken,
		// EncryptionKey carries the WRAPPED DEK so the node can unwrap and open the
		// source template's encrypted container before restoring; KekId names the
		// KEK that wrapped it (non-secret). Both empty for a plaintext template.
		// The wrapped DEK is never logged.
		EncryptionKey: wrappedDEK,
		KekId:         kekID,
		// VitalsLabels lets the node label the sandbox's guest telemetry with the
		// claim/pool/workspace/namespace identity. Object names, never secrets; nil
		// leaves the sandbox unlabeled.
		VitalsLabels: labels,
	})
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("forkd fork on %s: %w", node.Name, err)
	}
	return &forkResult{
		SandboxID:  resp.SandboxId,
		Endpoint:   clientEndpoint(node, resp.Endpoint),
		ForkTimeMs: resp.ForkTimeMs,
	}, nil
}

// forkRunningOnNode asks forkd to checkpoint a running sandbox and fork it.
// apiToken is the NEW sandbox's own bearer token (the source's token does
// not open the fork); it is never logged.
// volumes are the resolved VolumeMounts for the new fork (with overrides
// applied). A live fork inherits the source's already-attached drives, and the
// ForkRunning RPC does not yet carry volume mounts, so they are accepted and
// validated here but not sent on the wire until the live-fork drive-rebind
// path lands (Task 3). Names, mount paths, and policies are config, safe to
// log.
func (r *SandboxForkReconciler) forkRunningOnNode(ctx context.Context, node *NodeInfo, sourceSandboxID, newSandboxID string, pauseSource bool, volumes []*forkdpb.VolumeMount, apiToken string) (*forkRunningResult, error) {
	_ = volumes
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return nil, err
	}
	resp, err := forkdpb.NewForkDaemonClient(conn).ForkRunning(ctx, &forkdpb.ForkRunningRequest{
		SourceSandboxId: sourceSandboxID,
		NewSandboxId:    newSandboxID,
		PauseSource:     pauseSource,
		ApiToken:        apiToken,
	})
	if err != nil {
		return nil, fmt.Errorf("forkd fork-running on %s: %w", node.Name, err)
	}
	return &forkRunningResult{
		SandboxID:    resp.SandboxId,
		Endpoint:     clientEndpoint(node, resp.Endpoint),
		ForkTimeMs:   resp.ForkTimeMs,
		CheckpointMs: resp.CheckpointTimeMs,
	}, nil
}

// pullTemplateOnNode asks the forkd on node to pull a template's snapshot from a
// holder's CAS surface over the node's mTLS gRPC. sourceURL is the holder's CAS
// base URL, digest the manifest digest to verify against, and token the shared
// peer credential the holder requires. The token is a secret value: it is sent
// only over the mTLS channel and is NEVER logged or recorded as a span
// attribute. The source URL and digest are content addresses, safe to log.
func (r *SandboxPoolReconciler) pullTemplateOnNode(ctx context.Context, node *NodeInfo, templateID, digest, sourceURL, token string) error {
	conn, err := r.NodeRegistry.GetConnection(node.Name)
	if err != nil {
		return err
	}
	if _, err := forkdpb.NewForkDaemonClient(conn).PullTemplate(ctx, &forkdpb.PullTemplateRequest{
		TemplateId:     templateID,
		ManifestDigest: digest,
		SourceUrl:      sourceURL,
		// PullToken is the credential the holder's CAS surface requires. Never
		// logged.
		PullToken: token,
	}); err != nil {
		return fmt.Errorf("forkd pull template on %s: %w", node.Name, err)
	}
	return nil
}

// clientEndpoint prefers the node's HTTP sandbox API; the engine-reported
// endpoint is an internal placeholder until guest networking exists.
func clientEndpoint(node *NodeInfo, engineEndpoint string) string {
	if node.HTTPEndpoint != "" {
		return node.HTTPEndpoint
	}
	return engineEndpoint
}

func toEnvVars(m map[string]string) []*forkdpb.EnvVar {
	vars := make([]*forkdpb.EnvVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.EnvVar{Key: k, Value: v})
	}
	return vars
}

// toNetworkConfig maps the template's NetworkPolicy onto the proto
// NetworkConfig forkd consumes. A nil policy (template declares no network)
// yields nil, leaving the fork un-networked. The allowlist is passed through
// verbatim; forkd splits it into enforceable IP:port entries and name-based
// entries (the latter logged as not-yet-enforced), so the controller does not
// validate or filter it here.
func toNetworkConfig(n *v1alpha1.NetworkPolicy) *forkdpb.NetworkConfig {
	if n == nil {
		return nil
	}
	return &forkdpb.NetworkConfig{
		EgressPolicy: string(n.Egress),
		AllowList:    n.Allow,
		BlockNetwork: n.BlockNetwork,
		AllowCidrs:   n.AllowCIDRs,
		Inbound:      string(n.Inbound),
		InboundCidrs: n.InboundCIDRs,
	}
}

// volumeMounts translates a template's Volumes into the proto VolumeMounts the
// Fork RPC carries, applying any per-name VolumeOverride to the fork policy.
// The node resolves the backing source and prepares the drive per policy; the
// controller only passes the spec through. Names, mount paths, policies, and
// sizes are config (no secrets), safe to log. A nil result means the template
// declared no volumes.
func volumeMounts(templateVols []v1alpha1.SandboxVolume, overrides []v1alpha1.VolumeOverride) []*forkdpb.VolumeMount {
	if len(templateVols) == 0 {
		return nil
	}
	overrideMap := make(map[string]v1alpha1.ForkPolicy, len(overrides))
	for _, o := range overrides {
		overrideMap[o.Name] = o.ForkPolicy
	}
	mounts := make([]*forkdpb.VolumeMount, 0, len(templateVols))
	for _, vol := range templateVols {
		policy := vol.ForkPolicy
		if override, ok := overrideMap[vol.Name]; ok {
			policy = override
		}
		mounts = append(mounts, &forkdpb.VolumeMount{
			Name:       vol.Name,
			MountPath:  vol.MountPath,
			ReadOnly:   vol.ReadOnly,
			ForkPolicy: string(policy),
			Size:       vol.Size,
		})
	}
	return mounts
}

// claimVitalsLabels builds the proto VitalsLabels the Fork RPC carries so the
// node can label the sandbox's Layer 3 guest telemetry (issue #164) with the
// control-plane identity: the claim name, its pool, its bound workspace (empty
// when the claim has no workspaceRef), and the namespace. Every field is a
// Kubernetes object name, never a secret value. Always non-nil so a poolless or
// workspaceless claim still labels what it can.
func claimVitalsLabels(claim *v1alpha1.SandboxClaim) *forkdpb.VitalsLabels {
	workspace := ""
	if claim.Spec.WorkspaceRef != nil {
		workspace = claim.Spec.WorkspaceRef.Name
	}
	return &forkdpb.VitalsLabels{
		Claim:     claim.Name,
		Pool:      claim.Spec.PoolRef.Name,
		Workspace: workspace,
		Namespace: claim.Namespace,
	}
}

func toSecretVars(m map[string]string) []*forkdpb.SecretVar {
	vars := make([]*forkdpb.SecretVar, 0, len(m))
	for k, v := range m {
		vars = append(vars, &forkdpb.SecretVar{Key: k, Value: v})
	}
	return vars
}
