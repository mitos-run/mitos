package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxPool is the v1 consolidated pool kind. It inlines the former
// SandboxTemplate into spec.template (ADR 0007); spec.templateRef survives as
// the optional reuse alternative (the Deployment-embeds-PodSpec pattern). A pool
// sets EXACTLY ONE of spec.template or spec.templateRef.
//
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.warm.min,statuspath=.status.readySnapshots
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readySnapshots`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.template.image`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired pool configuration: the template (inline or by
	// reference), the snapshot fan-out, the warm-pod autoscaler, and placement.
	Spec SandboxPoolSpec `json:"spec"`

	// Status reports snapshot and warm-pool readiness.
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// SandboxPoolSpec is the v1 pool spec. Exactly one of Template (inline) or
// TemplateRef (a reference to a shared template-shaped object) is set.
type SandboxPoolSpec struct {
	// Template is the inline pool template carrying every field a v1alpha1
	// SandboxTemplate carried (image, init, command, env, resources, volumes,
	// network, encrypted). It is the common path; mutually exclusive with
	// TemplateRef. Exactly one of Template or TemplateRef must be set.
	// +optional
	Template *PoolTemplateSpec `json:"template,omitempty"`

	// TemplateRef references a shared template-shaped object by name, for the
	// rarer case of several pools sharing one template definition. Mutually
	// exclusive with Template. Exactly one of Template or TemplateRef must be set.
	// +optional
	TemplateRef *LocalObjectReference `json:"templateRef,omitempty"`

	// Snapshots configures the per-node snapshot fan-out: how many warm snapshot
	// restores each node holds, the prefetch posture, and the refresh schedule.
	// It folds the v1alpha1 snapshotAfter, snapshotDelay, scaleDownAfterSnapshot,
	// and snapshotStorage fields into one block.
	// +optional
	Snapshots *PoolSnapshots `json:"snapshots,omitempty"`

	// Warm is the husk-pod autoscaler: the floor, ceiling, and target pending
	// headroom of DORMANT warm husk pods. It re-homes the v1alpha1
	// replicas/autoscale fields into one block (warm.min carries the fixed
	// replicas count for back-compat).
	// +optional
	Warm *PoolWarm `json:"warm,omitempty"`

	// DrainPolicy governs an active sandbox when its backing husk pod is lost
	// (drain, eviction, deletion). Kill (the default) re-pends the sandbox onto a
	// replacement dormant slot; Checkpoint attempts a live-VM snapshot first where
	// the VMM still runs, then re-pends. Carried unchanged from v1alpha1.
	// +kubebuilder:validation:Enum=Kill;Checkpoint
	// +kubebuilder:default=Kill
	// +optional
	DrainPolicy HuskDrainPolicy `json:"drainPolicy,omitempty"`

	// Placement pins this pool's husk pods (and the sandbox VMs they run) to a
	// dedicated set of nodes for hard tenant separation (issue #172). Carried
	// unchanged from v1alpha1.
	// +optional
	Placement *PoolPlacement `json:"placement,omitempty"`

	// CPUPinning configures dynamic post-ready CPU pinning and a launch-time
	// scheduling-priority bump for this pool's sandbox VMs (issue #168). Carried
	// unchanged from v1alpha1.
	// +optional
	CPUPinning *CPUPinningSpec `json:"cpuPinning,omitempty"`
}

// PoolTemplateSpec is the inline pool template: every field of the v1alpha1
// SandboxTemplateSpec (now built into the pool, ADR 0007) plus the new
// DefaultBudget inherited by sandboxes from this pool.
type PoolTemplateSpec struct {
	// Image is the OCI base image the template snapshot is built from.
	Image string `json:"image"`

	// Init is the ordered list of build-time shell commands run inside the
	// booting template VM. Pool-build only; never per-sandbox.
	// +optional
	Init []string `json:"init,omitempty"`

	// BuildSteps is the ordered, declarative build recipe (issue #220), the
	// code-first alternative to Init. Carried unchanged from v1alpha1.
	// +optional
	BuildSteps []BuildStep `json:"buildSteps,omitempty"`

	// Command overrides the container entrypoint inside the sandbox.
	// +optional
	Command []string `json:"command,omitempty"`

	// Env are environment variables baked into the template at build time.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources is the per-sandbox CPU and memory request (and optional GPU).
	// +optional
	Resources SandboxResources `json:"resources,omitempty"`

	// Volumes are the backing drives attached to each sandbox, with per-volume
	// fork policies (Fresh, Share, Clone, Snapshot).
	// +optional
	Volumes []SandboxVolume `json:"volumes,omitempty"`

	// Network is the per-sandbox egress/inbound posture enforced by the per-tap
	// nftables datapath. Mapped from the v1alpha1 networkPolicy field.
	// +optional
	Network *NetworkPolicy `json:"network,omitempty"`

	// Encrypted requests at-rest encryption of the template snapshot and every
	// fork built from it. Carried unchanged from v1alpha1.
	// +kubebuilder:default=false
	// +optional
	Encrypted bool `json:"encrypted,omitempty"`

	// MinIsolationTier requires the sandbox to be scheduled only onto a node
	// whose isolation assurance meets this floor (issue #40). Carried unchanged
	// from v1alpha1.
	// +kubebuilder:validation:Enum=hardware-kvm;pvm;gvisor
	// +optional
	MinIsolationTier string `json:"minIsolationTier,omitempty"`

	// RequireHardwareKvm is a convenience equivalent to
	// minIsolationTier=hardware-kvm (issue #40). Carried unchanged from v1alpha1.
	// +kubebuilder:default=false
	// +optional
	RequireHardwareKvm bool `json:"requireHardwareKvm,omitempty"`

	// DefaultBudget is the pool's default capability budget inherited by a
	// Sandbox created from this pool when the Sandbox sets no budget of its own
	// (v2-spec section 3). NEW v2 surface; default empty (no budget). Runtime
	// enforcement is issue #25.
	// +optional
	DefaultBudget *SandboxBudget `json:"defaultBudget,omitempty"`

	// Workload declares a long-running process the build starts AFTER init and
	// keeps running while it takes the snapshot, so a fork wakes with the app
	// already serving on its port (issue #460). The build gates the snapshot on
	// the workload's ready probe, and the workload is excluded from the per-fork
	// userspace reset signal so it survives forks. Distinct from Command (an
	// exec-time entrypoint default that is never started during the build).
	// +optional
	Workload *WorkloadSpec `json:"workload,omitempty"`

	// WarmKernel, when true, has the template build run one trivial run_code
	// cell AFTER init and the workload start and BEFORE the snapshot, so the
	// code-interpreter kernel is captured live and every fork skips its ~5s
	// lazy kernel start on the first run_code. The warmup cell draws no
	// randomness (the kernel's Python PRNGs stay unseeded in the snapshot;
	// each fork seeds fresh after the per-fork CRNG reseed, see
	// docs/fork-correctness.md) and fails open: an image without the kernel
	// logs and builds unchanged, so non-python pools may leave this set.
	// +kubebuilder:default=false
	// +optional
	WarmKernel bool `json:"warmKernel,omitempty"`
}

// WorkloadSpec declares a serving workload captured running in the template
// snapshot (issue #460). The build starts Command in its own session so it
// outlives the build's exec, waits for Ready, then snapshots.
type WorkloadSpec struct {
	// Command is the serving process, run through the shell inside the guest.
	Command []string `json:"command"`

	// Env are environment variables for the workload process. Non-secret only;
	// secret values are injected per fork, never baked into the snapshot.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Ready is the HTTP gate the build waits on before snapshotting; without it
	// the build snapshots as soon as the workload is started.
	// +optional
	Ready *HTTPReadyProbe `json:"ready,omitempty"`
}

// HTTPReadyProbe is the HTTP check the build polls inside the guest until the
// workload is listening, so the snapshot captures a serving app (issue #460).
type HTTPReadyProbe struct {
	// Port is the guest TCP port the workload listens on.
	Port int32 `json:"port"`

	// Path is the request path; defaults to "/".
	// +optional
	Path string `json:"path,omitempty"`

	// Expect is the HTTP status that counts as ready; defaults to 200.
	// +optional
	Expect int32 `json:"expect,omitempty"`

	// TimeoutSeconds bounds the readiness wait before the build fails; defaults
	// to 120.
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// PoolSnapshots is the per-node snapshot fan-out configuration. It folds the
// v1alpha1 snapshotAfter / snapshotDelay / scaleDownAfterSnapshot /
// snapshotStorage fields into one block (the v2 presentation regrouping; the
// conversion preserves every v1 value, see docs/api/v2-migration.md).
type PoolSnapshots struct {
	// ReplicasPerNode is the number of warm snapshot restores each holder node
	// keeps. It carries the v1alpha1 spec.replicas value on conversion.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReplicasPerNode int32 `json:"replicasPerNode,omitempty"`

	// Prefetch is the snapshot prefetch posture (for example "full"): how much of
	// the snapshot is pulled onto a node before it is considered ready.
	// +optional
	Prefetch string `json:"prefetch,omitempty"`

	// SnapshotAfter is the trigger condition before a snapshot is taken (for
	// example Ready). Re-homed from v1alpha1 spec.snapshotAfter.
	// +optional
	SnapshotAfter SnapshotTrigger `json:"snapshotAfter,omitempty"`

	// SnapshotDelay is the delay after the trigger before the snapshot is taken,
	// allowing init scripts to finish. Re-homed from v1alpha1 spec.snapshotDelay.
	// +optional
	SnapshotDelay *metav1.Duration `json:"snapshotDelay,omitempty"`

	// ScaleDownAfterSnapshot scales down the source sandbox after the snapshot is
	// taken. Re-homed from v1alpha1 spec.scaleDownAfterSnapshot.
	// +optional
	ScaleDownAfterSnapshot bool `json:"scaleDownAfterSnapshot,omitempty"`

	// Storage is where snapshot artifacts are stored on the node. Re-homed from
	// v1alpha1 spec.snapshotStorage.
	// +optional
	Storage string `json:"storage,omitempty"`

	// Refresh schedules a periodic snapshot rebuild (for example a nightly cron),
	// so a pool's snapshots track a moving base image. NEW v2 surface; empty
	// leaves snapshots fixed until the template changes.
	// +optional
	Refresh *PoolSnapshotRefresh `json:"refresh,omitempty"`
}

// PoolSnapshotRefresh schedules periodic snapshot rebuilds for a pool.
type PoolSnapshotRefresh struct {
	// Schedule is a cron expression (for example "0 4 * * *") for the rebuild.
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// PoolWarm is the husk-pod autoscaler block. It re-homes the v1alpha1
// replicas/autoscale fields: Min carries the fixed replicas floor for
// back-compat, and the v1alpha1 autoscale.minWarm/maxWarm/targetSpare/
// scaleDownCooldownSeconds map onto Min/Max/TargetPending and Cooldown.
type PoolWarm struct {
	// Min is the floor: the dormant warm husk pod count held when fully idle. It
	// carries the v1alpha1 autoscale.minWarm (or, when no autoscale was set, the
	// fixed spec.replicas) on conversion.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min int32 `json:"min,omitempty"`

	// Max is the ceiling: the dormant warm husk pod count never exceeds this. It
	// carries the v1alpha1 autoscale.maxWarm on conversion. Zero means no
	// autoscaler ceiling (the fixed-pool back-compat shape).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Max int32 `json:"max,omitempty"`

	// TargetPending is the headroom of spare dormant pods kept ready on top of
	// the in-use count, so a claim burst hits a warm pod instead of cold-starting.
	// It carries the v1alpha1 autoscale.targetSpare on conversion.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TargetPending int32 `json:"targetPending,omitempty"`

	// CooldownSeconds is the anti-thrash window after a scale-down before the pool
	// may scale down again. It carries the v1alpha1
	// autoscale.scaleDownCooldownSeconds on conversion.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty"`
}

// SandboxPoolList is the list type for SandboxPool.
//
// +kubebuilder:object:root=true
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
