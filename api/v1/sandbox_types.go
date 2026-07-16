package v1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Sandbox is the v1 consolidated run-axis kind: one running sandbox. It
// folds the former SandboxClaim and SandboxFork into one kind (ADR 0007). Its
// source is a required oneof selecting the origin (a pool snapshot, a live
// sandbox to fork, or a workspace revision to resume); replicas carries the
// fork fan-out. A Sandbox with source.poolRef and replicas 1 is the old claim;
// a Sandbox with source.fromSandbox and replicas N is the old fork.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.pod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired sandbox: its source, fan-out, lifetime, budget, and
	// the workspace/secret/network bindings carried from the v1alpha1 claim/fork.
	Spec SandboxSpec `json:"spec"`

	// Status reports the sandbox phase, endpoint, husk pod, produced revision,
	// budget spend, startup latency, conditions, and per-child status when
	// replicas exceeds one.
	Status SandboxStatus `json:"status,omitempty"`
}

// SandboxSpec is the consolidated run-axis spec.
type SandboxSpec struct {
	// Source is the required discriminated union selecting the sandbox origin.
	// Exactly one of poolRef, fromSandbox, or fromRevision is set.
	Source SandboxSource `json:"source"`

	// Replicas is the fork fan-out: 1 (the default) is a single sandbox (the old
	// claim), and a value greater than 1 with source.fromSandbox produces that
	// many indexed sibling children (the old fork). Carried from
	// SandboxFork.replicas.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Resume selects whether a fromSandbox/fromRevision source restores warm VM
	// memory (memory, the default) or only the filesystem (filesystem). A
	// cross-principal handoff forces filesystem (the memory-snapshot principal
	// binding, ADR 0002, docs/fork-correctness.md). NEW v2 surface with a
	// documented default; it has no v1 source field.
	// +kubebuilder:validation:Enum=memory;filesystem
	// +kubebuilder:default=memory
	// +optional
	Resume ResumeMode `json:"resume,omitempty"`

	// Env are environment variables delivered to the sandbox. Carried unchanged
	// from SandboxClaim.env.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Secrets are the secret mounts delivered to the sandbox. Carried unchanged
	// from SandboxClaim.secrets.
	// +optional
	Secrets []SecretMount `json:"secrets,omitempty"`

	// VolumeOverrides override per-volume fork policies for this sandbox. Carried
	// unchanged from SandboxClaim.volumeOverrides / SandboxFork.volumeOverrides.
	// +optional
	VolumeOverrides []VolumeOverride `json:"volumeOverrides,omitempty"`

	// SecretInheritance governs whether a fork (source.fromSandbox) inherits the
	// source sandbox's in-memory secrets. reissue (the default) gives each fork
	// fresh credentials; inherit requires source opt-in and duplicates guest
	// memory including secret values (docs/fork-correctness.md section 3). It
	// replaces and inverts the v1alpha1 SandboxFork.allowSecretInheritance
	// boolean (false -> reissue, true -> inherit).
	// +kubebuilder:validation:Enum=reissue;inherit
	// +kubebuilder:default=reissue
	// +optional
	SecretInheritance SecretInheritanceMode `json:"secretInheritance,omitempty"`

	// WorkspaceRef binds this sandbox to a durable Workspace (single-writer).
	// Carried unchanged from SandboxClaim.workspaceRef.
	// +optional
	WorkspaceRef *LocalObjectReference `json:"workspaceRef,omitempty"`

	// ServiceAccount is the principal this sandbox runs as: the identity workspace
	// grants are evaluated against and a memory snapshot is bound to. Carried
	// unchanged from SandboxClaim.serviceAccount.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// NodeName is an optional node preference for placement. Carried unchanged
	// from SandboxClaim.nodeName.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// Network carries per-sandbox network additions on top of the pool policy.
	// NEW v2 surface (extraAllow); default empty.
	// +optional
	Network *SandboxNetwork `json:"network,omitempty"`

	// Expose declares that a guest port should be reachable through the Mitos
	// Expose edge proxy at a per-sandbox subdomain. Optional; absent means the
	// sandbox is not exposed.
	// +optional
	Expose *SandboxExpose `json:"expose,omitempty"`

	// Budget is the capability budget for runtime self-service: the five maxima
	// (maxForks, maxCheckpoints, maxCpuSeconds, maxLifetimeExtension,
	// maxEgressBytes). NEW v2 surface (v2-spec section 3); defaults from the
	// pool's defaultBudget. Runtime enforcement is issue #25.
	// +optional
	Budget *SandboxBudget `json:"budget,omitempty"`

	// Lifetime carries the wall-clock and idle limits and the terminate-time
	// outputs/snapshot directives. It re-homes the v1alpha1 SandboxClaim
	// timeout/idleTimeout/outputs/checkpointOnTerminate/ttlSecondsAfterFinished.
	// +optional
	Lifetime *SandboxLifetime `json:"lifetime,omitempty"`
}

// SandboxSource is the discriminated union selecting a sandbox's origin. Exactly
// one of PoolRef, FromSandbox, or FromRevision is set.
type SandboxSource struct {
	// PoolRef starts a fresh sandbox from a pool snapshot (the old SandboxClaim
	// path). Maps from SandboxClaim.poolRef.
	// +optional
	PoolRef *LocalObjectReference `json:"poolRef,omitempty"`

	// FromSandbox forks a live sandbox (the old SandboxFork path). Maps from
	// SandboxFork.sourceRef. With replicas greater than 1 it fans out into that
	// many indexed sibling children.
	// +optional
	FromSandbox *FromSandboxSource `json:"fromSandbox,omitempty"`

	// FromRevision resumes a sandbox from a workspace revision (lineage resume).
	// NEW v2 surface; it has no v1 source.
	// +optional
	FromRevision *FromRevisionSource `json:"fromRevision,omitempty"`
}

// FromSandboxSource forks a live sandbox by name.
type FromSandboxSource struct {
	// Name is the live sandbox to fork.
	Name string `json:"name"`

	// PauseSource pauses the source sandbox during the fork checkpoint. Reduces
	// checkpoint time but briefly interrupts the source. Carried unchanged from
	// SandboxFork.pauseSource.
	// +optional
	PauseSource bool `json:"pauseSource,omitempty"`
}

// FromRevisionSource resumes a sandbox from a workspace revision (NEW v2
// lineage-resume surface).
type FromRevisionSource struct {
	// Workspace is the durable workspace the revision belongs to.
	Workspace string `json:"workspace"`

	// Revision is the workspace revision to resume (for example "rev-41").
	Revision string `json:"revision"`
}

// SandboxNetwork carries per-sandbox network additions on top of the pool's
// network policy (NEW v2 surface).
type SandboxNetwork struct {
	// ExtraAllow adds host:port egress destinations on top of the pool template's
	// allowlist for this sandbox only. Default empty.
	// +optional
	ExtraAllow []string `json:"extraAllow,omitempty"`
}

// SandboxExpose configures the per-sandbox expose route (Mitos Expose slice 2b).
type SandboxExpose struct {
	// Port is the guest TCP port to expose.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// Label is the single subdomain label the route is served at (for example
	// "openclaw" in openclaw.<expose-domain>). Must be a single DNS label.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Label string `json:"label"`
	// Sharing is the access tier. Slice 2b carries the value through to the
	// proxy as an opaque string (the proxy enforces "link" today; the full
	// ladder is slice 4). Defaults to private.
	// +kubebuilder:validation:Enum=private;link;org;authenticated;public
	// +kubebuilder:default=private
	// +optional
	Sharing string `json:"sharing,omitempty"`
	// Network is a CIDR allowlist evaluated on every request before any
	// identity check. An empty list means all source IPs are allowed.
	// +optional
	Network []string `json:"network,omitempty"`
	// ForwardAuthURL is an optional external forward-auth endpoint. When set
	// the proxy makes a subrequest to this URL; a non-2xx response is returned
	// to the client and a 2xx response's identity headers are trusted as the
	// caller identity.
	// +optional
	ForwardAuthURL string `json:"forwardAuthURL,omitempty"`
	// AllowedPrincipals is an optional audience allowlist by email. When set
	// only callers whose verified email is in this list may access the route.
	// Rejected as a misconfiguration on the public tier (no identity).
	// +optional
	AllowedPrincipals []string `json:"allowedPrincipals,omitempty"`
	// AllowedEmailDomains is an optional audience allowlist by email domain
	// (exact, case-folded, registrable domain, not a suffix match). Only
	// callers with a verified email in one of these domains may access the
	// route. Unverified email is rejected.
	// +optional
	AllowedEmailDomains []string `json:"allowedEmailDomains,omitempty"`
}

// SandboxBudget is the capability budget for runtime self-service (v2-spec
// section 3, issue #25). All five maxima are optional; an unset maximum is
// unbounded. NEW v2 surface; runtime enforcement is issue #25.
type SandboxBudget struct {
	// MaxForks bounds the self-initiated forks (depth-aggregate) the sandbox may
	// create at runtime.
	// +optional
	MaxForks *int32 `json:"maxForks,omitempty"`

	// MaxCheckpoints bounds the self-initiated checkpoints the sandbox may take.
	// +optional
	MaxCheckpoints *int32 `json:"maxCheckpoints,omitempty"`

	// MaxCpuSeconds bounds the cumulative CPU seconds the sandbox may consume.
	// +optional
	MaxCpuSeconds *int64 `json:"maxCpuSeconds,omitempty"`

	// MaxLifetimeExtension bounds the total lifetime extension the sandbox may
	// request at runtime.
	// +optional
	MaxLifetimeExtension *metav1.Duration `json:"maxLifetimeExtension,omitempty"`

	// MaxEgressBytes bounds the cumulative egress bytes the sandbox may send. It
	// is a Kubernetes quantity (for example "1Gi").
	// +optional
	MaxEgressBytes *resource.Quantity `json:"maxEgressBytes,omitempty"`
}

// SandboxLifetime carries the wall-clock and idle limits and the terminate-time
// directives, re-homing the v1alpha1 SandboxClaim lifetime fields under one
// block.
type SandboxLifetime struct {
	// TTL is the maximum wall-clock lifetime of the sandbox (maxLifetime). Zero
	// means no limit. Maps from SandboxClaim.timeout.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// IdleTimeout reaps the sandbox after this much time with no exec or file
	// activity. Zero means no idle limit. Maps from SandboxClaim.idleTimeout.
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// TTLSecondsAfterFinished bounds how long a finished sandbox lingers in the
	// API before the garbage collector reaps it from etcd. Maps from
	// SandboxClaim.ttlSecondsAfterFinished, re-homed under lifetime.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// OnTerminate declares what the terminate step captures (outputs) and whether
	// it retains a snapshot. Re-homes SandboxClaim.outputs and generalizes
	// SandboxClaim.checkpointOnTerminate.
	// +optional
	OnTerminate *OnTerminate `json:"onTerminate,omitempty"`
}

// OnTerminate declares the terminate-time outputs and snapshot retention.
type OnTerminate struct {
	// Outputs declares what the terminate-with-outputs step captures from the
	// sandbox /workspace into the new WorkspaceRevision. Carried unchanged in
	// shape from SandboxClaim.outputs (path, diff, git).
	// +optional
	Outputs []OutputSpec `json:"outputs,omitempty"`

	// Snapshot is a retention directive (for example "retain-last-3") that
	// generalizes the v1alpha1 SandboxClaim.checkpointOnTerminate boolean: a
	// non-empty value requests a memory snapshot paired with the new revision so
	// the workspace head becomes resumable.
	// +optional
	Snapshot string `json:"snapshot,omitempty"`
}

// ResumeMode selects warm-memory vs filesystem-only restore for a
// fromSandbox/fromRevision source.
type ResumeMode string

const (
	// ResumeMemory restores warm VM memory and the filesystem (the default).
	ResumeMemory ResumeMode = "memory"
	// ResumeFilesystem restores only the filesystem; a cross-principal handoff
	// forces this.
	ResumeFilesystem ResumeMode = "filesystem"
)

// SecretInheritanceMode governs whether a fork inherits the source's in-memory
// secrets.
type SecretInheritanceMode string

const (
	// SecretReissue gives each fork fresh credentials (the safer default).
	SecretReissue SecretInheritanceMode = "reissue"
	// SecretInherit duplicates the source's in-memory secrets into the fork;
	// requires source opt-in.
	SecretInherit SecretInheritanceMode = "inherit"
)

// Condition vocabulary shared between the controller (which writes fork
// status conditions) and the SaaS control plane (which translates them into
// public HTTP envelopes). Both sides MUST use these symbols: a renamed
// literal on either side would silently downgrade the dedicated
// secret-inheritance 403 into the generic rejection 409.
const (
	// ConditionRejected is the condition type a terminal fork rejection carries.
	ConditionRejected = "Rejected"
	// ReasonSecretInheritanceDenied is the Rejected reason for the default-deny
	// secret-inheritance gate (fork asked to inherit without the explicit opt-in).
	ReasonSecretInheritanceDenied = "SecretInheritanceDenied"
)

// DefaultVMID is the intra-pod identity of the single primary VM a husk host
// pod backs on the default single-VM path. The controller stamps it into
// SandboxStatus.VMID for a normal 1:1 husk-backed sandbox so the (Pod, VMID)
// host mapping is populated and correct before multi-VM co-location exists. It
// MUST match the husk stub's primary vmID key (internal/husk defaultVMID).
const DefaultVMID = "default"

// SandboxStatus consolidates SandboxClaimStatus and SandboxForkStatus.
type SandboxStatus struct {
	// Phase is the sandbox lifecycle phase. The phase-name set is carried from
	// v1alpha1 (Pending, Restoring, Ready, Terminating, Terminated, Failed);
	// Terminated is a terminal phase reaped by TTL. The v2 phase rename
	// (Hydrating, NodeLost) is deferred to a later task.
	// +optional
	Phase SandboxPhase `json:"phase,omitempty"`

	// Endpoint is the sandbox API address (host:port). Unchanged from v1alpha1.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Restarts counts how many times this sandbox's backing VM was destroyed and
	// replaced by a fresh one (a husk pod loss re-pended under DrainPolicy Kill).
	//
	// It is the DURABLE signal that the sandbox a caller is holding is no longer the
	// sandbox it created: the replacement re-activates from the POOL TEMPLATE, so every
	// process, every write to the guest filesystem, and the whole guest memory of the
	// previous instance are gone. The Ready condition also flips, but only transiently
	// (the next reconcile re-activates and sets Ready=True again), so a caller polling
	// the API would otherwise see an unbroken healthy sandbox and keep issuing calls
	// against state that no longer exists (mitos-run/mitos#870).
	//
	// Monotonic for the lifetime of the sandbox. Zero means the caller still holds the
	// original VM.
	// +optional
	Restarts int32 `json:"restarts,omitempty"`

	// LastRestartTime is when the backing VM was most recently destroyed and replaced.
	// Nil while Restarts is zero.
	// +optional
	LastRestartTime *metav1.Time `json:"lastRestartTime,omitempty"`

	// Pod is the husk host pod name backing the sandbox (for example
	// "heartbeat-7f3a-husk"), visible to kubectl, quotas, NetworkPolicy, and
	// OpenCost. NEW explicit v2 field. On the husk path the node is derivable
	// from the pod; on the raw-forkd path the node is carried in Node below.
	// In multi-VM mode a single host pod can back several same-tenant Sandbox
	// CRs at once: the pod is a shared runtime host BELOW the CRD, which stays
	// 1:1 with a VM, and each co-located Sandbox is distinguished by VMID
	// below. Pod stays the host-pod handle in both the single-VM and multi-VM
	// cases; it is never renamed to a per-VM identity.
	// +optional
	Pod string `json:"pod,omitempty"`

	// VMID is the per-VM identity WITHIN the host Pod above. It carries the
	// default (primary) identity for a one-VM host pod and a distinct value per
	// co-located VM when a host pod backs several same-tenant Sandboxes in
	// multi-VM mode. Together (Pod, VMID) map a Sandbox CR to its exact host
	// process, so "kubectl get sandbox X -o yaml" still traces a sandbox to
	// where it runs even though "kubectl get pods" no longer maps one pod to
	// one sandbox. The CRD stays 1:1 with a VM; VMID is the intra-pod
	// discriminator, not a second sandbox handle. See DefaultVMID for the
	// single-VM value. NEW v2 field.
	// +optional
	VMID string `json:"vmId,omitempty"`

	// Node is the node the sandbox VM runs on. Unchanged from v1alpha1. It is the
	// engine placement identity distinct from SandboxID: the GC orphan sweep,
	// NodeLost detection, and the terminate/idle engine calls key off it on the
	// raw-forkd path, where it is not derivable from a husk pod.
	// +optional
	Node string `json:"node,omitempty"`

	// SandboxID is the engine-side sandbox identifier. Unchanged from v1alpha1.
	// +optional
	SandboxID string `json:"sandboxID,omitempty"`

	// StartupLatencyMs is the fork/activation latency in milliseconds. Renamed
	// and rescaled from the v1alpha1 SandboxClaimStatus.forkTimeMicros.
	// +optional
	StartupLatencyMs int64 `json:"startupLatencyMs,omitempty"`

	// StartedAt is when the sandbox became Ready. Unchanged from v1alpha1.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the sandbox reached a terminal phase, driving the GC TTL
	// pass. Unchanged from v1alpha1.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Revision is the WorkspaceRevision produced on terminate. NEW v2 field.
	// +optional
	Revision string `json:"revision,omitempty"`

	// BudgetSpend reports the capability-budget accounting (forks, cpuSeconds,
	// ...). NEW v2 field (issue #25).
	// +optional
	BudgetSpend *SandboxBudgetSpend `json:"budgetSpend,omitempty"`

	// EffectiveBudget is the controller-computed, attenuated capability budget
	// this sandbox actually holds: the per-field minimum (intersection) of its own
	// resolved budget and its parent's effective-remaining budget when it is a
	// self-initiated fork (source.fromSandbox), or its resolved spec/pool budget
	// otherwise. It is depth-aggregate: because each level intersects with its
	// parent's remaining, a descendant can never hold a budget wider than the root
	// has left, so the whole fork subtree is bounded by the root (issue #25, the
	// never-widen attenuation invariant). A nil dimension means unlimited. This is
	// STATUS only; the controller never mutates the user-owned spec.budget. NEW v2
	// field.
	// +optional
	EffectiveBudget *SandboxBudget `json:"effectiveBudget,omitempty"`

	// ReadyReplicas is the ready child count for a replicas > 1 Sandbox. Maps
	// from SandboxForkStatus.readyForks.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Children is the per-replica status for a fan-out (replicas > 1) Sandbox.
	// Maps from SandboxForkStatus.forks.
	// +optional
	Children []SandboxChild `json:"children,omitempty"`

	// ForkSnapshotTaken is the exactly-once fork-snapshot guard for a replicas >
	// 1 Sandbox, carried so the controller-restart correctness holds. Maps from
	// SandboxForkStatus.forkSnapshotTaken.
	// +optional
	ForkSnapshotTaken bool `json:"forkSnapshotTaken,omitempty"`

	// CheckpointTime is when the fork checkpoint was taken. Maps from
	// SandboxForkStatus.checkpointTime.
	// +optional
	CheckpointTime *metav1.Time `json:"checkpointTime,omitempty"`

	// ForkStartedAt is when the husk fork fan-out began its first working
	// reconcile pass (the source pod resolved Ready and the controller started
	// the snapshot/spawn work). A hosted co-location fork is LEVEL-TRIGGERED and
	// reaches Ready across several ~1s requeue passes, so the wall-clock latency
	// of the whole fork is NOT observable inside one pass; persisting the start
	// timestamp lets the controller attribute the true end-to-end latency (from
	// here to Ready) even though the work is split across passes. Set exactly
	// once and never rewritten. Timing/observability only; it drives no fork
	// behavior. NEW field.
	// +optional
	ForkStartedAt *metav1.Time `json:"forkStartedAt,omitempty"`

	// ForkReconcilePasses counts the husk fork reconcile passes that advanced the
	// fan-out (each pass that reached a status write). For a level-triggered
	// controller the pass count is a first-class part of the latency breakdown:
	// control-plane round-trips (one ~1s requeue per pass) can dominate the
	// end-to-end fork latency, so the pass count says how much of the total is
	// requeue wait versus real per-stage work. Timing/observability only. NEW
	// field.
	// +optional
	ForkReconcilePasses int32 `json:"forkReconcilePasses,omitempty"`

	// Conditions are the typed conditions with observedGeneration. Unchanged from
	// v1alpha1.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SandboxChild is the per-replica status for a fan-out Sandbox (the old
// SandboxForkStatus.forks ForkInfo).
type SandboxChild struct {
	// Name is the child sandbox name.
	Name string `json:"name"`
	// SandboxID is the child's engine-side identifier.
	SandboxID string `json:"sandboxID"`
	// Endpoint is the child's sandbox API address.
	Endpoint string `json:"endpoint"`
	// Node is the node the child runs on.
	Node string `json:"node"`
	// Phase is the child's lifecycle phase.
	Phase SandboxPhase `json:"phase"`
	// StartupLatencyMs is the child's fork latency in milliseconds.
	// +optional
	StartupLatencyMs int64 `json:"startupLatencyMs,omitempty"`

	// Pod is the husk host pod backing this child when it was spawned as an
	// ADDITIONAL VM inside an existing (shared) source pod rather than getting its
	// own pod (the multi-VM fork routing, controller flag default off). Empty on
	// the default one-pod-per-child path, where SandboxID already names the child's
	// own pod. Together with VMID it maps a co-located child to its exact host
	// process, mirroring SandboxStatus.Pod / SandboxStatus.VMID. NEW v2 field.
	// +optional
	Pod string `json:"pod,omitempty"`

	// VMID is the per-VM identity of this child WITHIN Pod above when it was
	// spawned into a shared source pod (multi-VM fork routing). Empty on the
	// default one-pod-per-child path (the child owns its whole pod, so the pod
	// name is its handle). NEW v2 field.
	// +optional
	VMID string `json:"vmId,omitempty"`
}

// SandboxBudgetSpend reports the capability-budget accounting (issue #25).
type SandboxBudgetSpend struct {
	// Forks is the number of self-initiated forks spent.
	// +optional
	Forks int32 `json:"forks,omitempty"`
	// CpuSeconds is the cumulative CPU seconds spent.
	// +optional
	CpuSeconds int64 `json:"cpuSeconds,omitempty"`
}

// SandboxList is the list type for Sandbox.
//
// +kubebuilder:object:root=true
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
