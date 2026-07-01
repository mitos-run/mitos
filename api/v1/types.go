package v1

// Shared leaf types moved from api/v1alpha1 (type inventory, Stage 1).
// These types are referenced by sandbox_types.go and sandboxpool_types.go;
// they carry no object-root markers and are not registered with the scheme
// independently. The removed kinds (SandboxTemplate, the v1alpha1 SandboxPool,
// SandboxClaim, SandboxFork, ForkInfo, PoolAutoscaleSpec) are deleted; they
// have no host in v1.

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildStepType is the kind of a declarative build step.
type BuildStepType string

const (
	// BuildStepRun runs a shell command inside the booting template VM at build
	// time, exactly as an Init command does.
	BuildStepRun BuildStepType = "run"
	// BuildStepEnv bakes an environment variable into the template by exporting it
	// for the remaining build steps and persisting it to /etc/profile so it is
	// present in every fork. The value is part of the cache key.
	BuildStepEnv BuildStepType = "env"
	// BuildStepWorkdir creates and changes into a working directory for the
	// remaining build steps.
	BuildStepWorkdir BuildStepType = "workdir"
	// BuildStepCopy stages host files into the image. The actual file
	// materialization is performed by the build path (KVM gated); the step is
	// carried here so the cache key chains over the declared source and
	// destination. Source is the host path, Dest the in-image path.
	BuildStepCopy BuildStepType = "copy"
)

// BuildStep is one ordered step of a declarative template build (issue #220).
// Exactly one shape is meaningful per Type. The build path flattens run, env,
// and workdir steps into the in-VM init commands in order; copy steps are
// staged by the file path. The whole ordered list feeds the chained,
// content-addressed cache key so an unchanged prefix is reused.
type BuildStep struct {
	// Type selects the step kind: run, env, workdir, or copy.
	// +kubebuilder:validation:Enum=run;env;workdir;copy
	Type BuildStepType `json:"type"`

	// Run is the shell command for a run step.
	// +kubebuilder:validation:Optional
	Run string `json:"run,omitempty"`

	// EnvName and EnvValue are the variable name and value for an env step.
	// +kubebuilder:validation:Optional
	EnvName string `json:"envName,omitempty"`
	// +kubebuilder:validation:Optional
	EnvValue string `json:"envValue,omitempty"`

	// Workdir is the directory for a workdir step.
	// +kubebuilder:validation:Optional
	Workdir string `json:"workdir,omitempty"`

	// Source and Dest are the host source and in-image destination for a copy
	// step.
	// +kubebuilder:validation:Optional
	Source string `json:"source,omitempty"`
	// +kubebuilder:validation:Optional
	Dest string `json:"dest,omitempty"`
}

// InitCommands flattens a BuildStep slice into in-VM build-time commands.
// run, env, and workdir steps produce commands in order; copy steps stage
// files outside the VM and contribute no init command. When steps is empty,
// initCmds is returned unchanged.
func InitCommands(steps []BuildStep, initCmds []string) []string {
	if len(steps) == 0 {
		return initCmds
	}
	cmds := make([]string, 0, len(steps))
	for i := range steps {
		step := &steps[i]
		switch step.Type {
		case BuildStepRun:
			if step.Run != "" {
				cmds = append(cmds, step.Run)
			}
		case BuildStepEnv:
			cmds = append(cmds, fmt.Sprintf("export %s='%s'; printf 'export %s=%%s\\n' '%s' >> /etc/profile",
				step.EnvName, step.EnvValue, step.EnvName, step.EnvValue))
		case BuildStepWorkdir:
			if step.Workdir != "" {
				cmds = append(cmds, fmt.Sprintf("mkdir -p '%s'; cd '%s'", step.Workdir, step.Workdir))
			}
		case BuildStepCopy:
			// Copy is staged by the file path, not an in-VM command.
		}
	}
	return cmds
}

// SandboxResources is the per-sandbox CPU, memory, and optional GPU request.
type SandboxResources struct {
	CPU    resource.Quantity `json:"cpu,omitempty"`
	Memory resource.Quantity `json:"memory,omitempty"`

	// GPU requests GPU passthrough for the sandbox (issue #221). When set the
	// pool's sandboxes are scheduled ONLY onto GPU-capable nodes, and the
	// requested device count and type are recorded for metering. A GPU sandbox
	// is documented as NOT live-forkable while the device is attached.
	// +optional
	GPU *GPUResources `json:"gpu,omitempty"`
}

// GPUResources is the GPU request for a sandbox (issue #221).
type GPUResources struct {
	// Count is the number of GPUs to attach, exclusively, to the sandbox.
	// +kubebuilder:validation:Minimum=1
	Count int32 `json:"count"`

	// Type names the GPU SKU the pool requires (for example "nvidia-a100").
	// +optional
	Type string `json:"type,omitempty"`
}

// ForkPolicy governs how a volume is handled when a sandbox is forked.
type ForkPolicy string

const (
	ForkPolicyFresh    ForkPolicy = "Fresh"
	ForkPolicyShare    ForkPolicy = "Share"
	ForkPolicyClone    ForkPolicy = "Clone"
	ForkPolicySnapshot ForkPolicy = "Snapshot"
)

// SandboxVolume is a backing drive attached to each sandbox.
type SandboxVolume struct {
	// Name identifies the volume; it becomes the host backing-file name and the
	// Firecracker drive id.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`
	// +kubebuilder:validation:MaxLength=64
	Name       string        `json:"name"`
	Size       string        `json:"size,omitempty"`
	Source     *VolumeSource `json:"source,omitempty"`
	ReadOnly   bool          `json:"readOnly,omitempty"`
	MountPath  string        `json:"mountPath,omitempty"`
	ForkPolicy ForkPolicy    `json:"forkPolicy,omitempty"`

	// For Snapshot fork policy: the CSI snapshot class to use.
	SnapshotClass string `json:"snapshotClass,omitempty"`

	// For persistent volumes: the storage class.
	StorageClass string `json:"storageClass,omitempty"`
}

// VolumeSource selects the backing data source for a sandbox volume.
type VolumeSource struct {
	S3  *S3VolumeSource  `json:"s3,omitempty"`
	GCS *GCSVolumeSource `json:"gcs,omitempty"`
	PVC *PVCVolumeSource `json:"pvc,omitempty"`
	Git *GitVolumeSource `json:"git,omitempty"`
}

// S3VolumeSource seeds a volume from an S3-compatible bucket.
type S3VolumeSource struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	Region string `json:"region,omitempty"`
}

// GCSVolumeSource seeds a volume from a GCS bucket.
type GCSVolumeSource struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

// PVCVolumeSource seeds a volume from a PersistentVolumeClaim.
type PVCVolumeSource struct {
	ClaimName string `json:"claimName"`
}

// GitVolumeSource seeds a volume by cloning a git repository.
type GitVolumeSource struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// EgressPolicy is the default verdict for sandbox egress traffic.
type EgressPolicy string

const (
	EgressDeny  EgressPolicy = "deny"
	EgressAllow EgressPolicy = "allow"
)

// InboundPolicy governs unsolicited inbound connections to the guest.
type InboundPolicy string

const (
	// InboundDeny drops every unsolicited inbound connection to the guest. This
	// is the secure default; return traffic for the guest's own egress flows is
	// still accepted via the ct established,related rule.
	InboundDeny InboundPolicy = "deny"
	// InboundAllow accepts unsolicited inbound connections, optionally narrowed
	// to InboundCIDRs. Only meaningful for a sandbox that hosts a listener.
	InboundAllow InboundPolicy = "allow"
)

// NetworkPolicy is the per-sandbox network posture, threaded from the template
// through the Fork RPC to the per-tap nftables datapath (docs/networking.md).
type NetworkPolicy struct {
	Egress EgressPolicy `json:"egress,omitempty"`
	Allow  []string     `json:"allow,omitempty"`

	// BlockNetwork drops ALL egress for the sandbox, overriding Egress and the
	// allowlists. It is the total-deny primitive.
	BlockNetwork bool `json:"blockNetwork,omitempty"`

	// AllowCIDRs is the egress CIDR allowlist. Ignored when BlockNetwork is true.
	AllowCIDRs []string `json:"allowCidrs,omitempty"`

	// Inbound governs unsolicited inbound connections to the guest. Empty means
	// the secure default, deny-by-default.
	Inbound InboundPolicy `json:"inbound,omitempty"`

	// InboundCIDRs narrows an InboundAllow to source CIDRs. Only meaningful
	// when Inbound is allow.
	InboundCIDRs []string `json:"inboundCidrs,omitempty"`
}

// SnapshotTrigger is the condition before a snapshot is taken.
type SnapshotTrigger string

const (
	SnapshotAfterReady SnapshotTrigger = "Ready"
)

// HuskDrainPolicy governs what happens to an ACTIVE sandbox when its backing
// husk pod is lost (drain, eviction, deletion).
type HuskDrainPolicy string

const (
	// DrainKill is the default: re-pend the sandbox onto a replacement dormant
	// slot when the husk pod is lost.
	DrainKill HuskDrainPolicy = "Kill"
	// DrainCheckpoint attempts a live-VM snapshot first where the VMM still
	// runs, then re-pends. Degrades to Kill when the pod is already gone.
	DrainCheckpoint HuskDrainPolicy = "Checkpoint"
)

// CPUPinningSpec configures dynamic post-ready CPU pinning and launch-time
// scheduling priority for a pool's sandbox VMs (issue #168).
type CPUPinningSpec struct {
	// Enabled turns dynamic post-ready CPU pinning on for this pool.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Policy is the packing strategy: Pack or Spread.
	// +kubebuilder:validation:Enum=spread;pack
	// +kubebuilder:default=pack
	// +optional
	Policy CPUPinningPolicy `json:"policy,omitempty"`

	// SiblingPairing assigns both hyperthread siblings of a physical core to
	// the same vCPU. Defaults to true.
	// +kubebuilder:default=true
	// +optional
	SiblingPairing *bool `json:"siblingPairing,omitempty"`

	// LaunchRtPriority bumps the Firecracker vCPU threads to an elevated
	// scheduling priority during the activate window. Defaults to true.
	// +kubebuilder:default=true
	// +optional
	LaunchRtPriority *bool `json:"launchRtPriority,omitempty"`
}

// CPUPinningPolicy is the packing strategy for the post-ready pin plan.
type CPUPinningPolicy string

const (
	// CPUPinningPack consolidates forks onto as few physical cores as possible.
	CPUPinningPack CPUPinningPolicy = "pack"
	// CPUPinningSpread distributes forks across distinct physical cores.
	CPUPinningSpread CPUPinningPolicy = "spread"
)

// Normalized returns a copy of the spec with the documented defaults filled in.
// A nil receiver normalizes to disabled. The returned value's *bool fields are
// non-nil so SiblingPairingEnabled and LaunchRtPriorityEnabled read directly.
func (s *CPUPinningSpec) Normalized() CPUPinningSpec {
	out := CPUPinningSpec{Policy: CPUPinningPack}
	t := true
	out.SiblingPairing = &t
	rt := true
	out.LaunchRtPriority = &rt
	if s == nil {
		return out
	}
	out.Enabled = s.Enabled
	if s.Policy != "" {
		out.Policy = s.Policy
	}
	if s.SiblingPairing != nil {
		v := *s.SiblingPairing
		out.SiblingPairing = &v
	}
	if s.LaunchRtPriority != nil {
		v := *s.LaunchRtPriority
		out.LaunchRtPriority = &v
	}
	return out
}

// SiblingPairingEnabled reports whether sibling pairing is on (nil = default true).
func (s CPUPinningSpec) SiblingPairingEnabled() bool {
	return s.SiblingPairing == nil || *s.SiblingPairing
}

// LaunchRtPriorityEnabled reports whether the launch-window RT priority bump
// is on (nil = default true).
func (s CPUPinningSpec) LaunchRtPriorityEnabled() bool {
	return s.LaunchRtPriority == nil || *s.LaunchRtPriority
}

// PoolPlacement pins a pool's husk pods to a dedicated node set (issue #172).
type PoolPlacement struct {
	// NodeSelector is ANDed onto the husk pod so its sandbox VMs run only on
	// nodes carrying these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations let the husk pods schedule onto tainted dedicated nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// SandboxPoolStatus is the observed state of a SandboxPool.
type SandboxPoolStatus struct {
	ReadySnapshots   int32              `json:"readySnapshots"`
	TotalSnapshots   int32              `json:"totalSnapshots"`
	RestoringCount   int32              `json:"restoringCount"`
	LastSnapshotTime *metav1.Time       `json:"lastSnapshotTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	NodeDistribution map[string]int32   `json:"nodeDistribution,omitempty"`

	// TemplateDigest is the content-addressed manifest digest of the pool's
	// snapshot. A content address, safe to log.
	TemplateDigest string `json:"templateDigest,omitempty"`

	// TemplateBuildHash records the build identity (a content hash of the template
	// image, init/buildSteps, workload, volumes, resources and encryption flag) the
	// pool's snapshot was last built from. The controller rebuilds the snapshot when
	// the template's current build identity no longer matches this, so a
	// workload.command, env, or ready-probe edit re-runs the build instead of being
	// silently ignored (issue #475). A content address, safe to log.
	TemplateBuildHash string `json:"templateBuildHash,omitempty"`

	// DesiredWarm is the autoscaler's computed desired dormant pod count.
	DesiredWarm int32 `json:"desiredWarm,omitempty"`

	// LastScaleDownTime records when the pool last reduced its dormant pod count.
	LastScaleDownTime *metav1.Time `json:"lastScaleDownTime,omitempty"`
}

// OutputSpec is one terminate-with-outputs directive.
type OutputSpec struct {
	// Path narrows the captured revision to this /workspace subtree.
	// +optional
	Path string `json:"path,omitempty"`

	// Diff requests that the new revision record the content-hash diff against
	// the workspace head revision before it.
	// +optional
	Diff bool `json:"diff,omitempty"`

	// Git pushes the workspace spec.git.paths content to a rendezvous remote.
	// +optional
	Git *GitOutput `json:"git,omitempty"`
}

// GitOutput declares a git rendezvous push target for a terminate output.
type GitOutput struct {
	// Remote is the rendezvous git remote the workspace repo paths are pushed to.
	// +optional
	// +kubebuilder:validation:Pattern=`^(https://|http://|ssh://|git://|file://|[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:).+`
	Remote string `json:"remote,omitempty"`

	// Branch is the per-attempt branch the push lands on.
	// +optional
	Branch string `json:"branch,omitempty"`
}

// SecretMount delivers a Kubernetes Secret key to the sandbox as an env var
// or a mounted file.
type SecretMount struct {
	Name      string                   `json:"name"`
	SecretRef corev1.SecretKeySelector `json:"secretRef"`
	EnvVar    string                   `json:"envVar,omitempty"`
	MountPath string                   `json:"mountPath,omitempty"`
}

// VolumeOverride overrides the fork policy for a named volume on this sandbox.
type VolumeOverride struct {
	Name       string     `json:"name"`
	ForkPolicy ForkPolicy `json:"forkPolicy"`
}

// SandboxPhase is the lifecycle phase of a sandbox.
type SandboxPhase string

const (
	SandboxPending     SandboxPhase = "Pending"
	SandboxRestoring   SandboxPhase = "Restoring"
	SandboxReady       SandboxPhase = "Ready"
	SandboxTerminating SandboxPhase = "Terminating"
	SandboxTerminated  SandboxPhase = "Terminated"
	SandboxFailed      SandboxPhase = "Failed"
)

// LocalObjectReference is a reference to an object in the same namespace by
// name.
type LocalObjectReference struct {
	Name string `json:"name"`
}
