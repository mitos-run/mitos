package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Husk pod warm-pool lifecycle (issue #18, slice 1).
//
// When --enable-husk-pods is set, a SandboxPool maintains a warm pool of
// pre-scheduled "husk" pods instead of building node-local snapshots. Each husk
// pod runs the dormant-VMM stub (cmd/husk-stub): it Prepares a dormant
// Firecracker VMM at start and waits on a control channel; a later migration
// slice (claim activation, slice 2) drives the in-place snapshot-load activation
// over that channel. Pre-scheduling pays the expensive Kubernetes work
// (scheduling, admission, netns, cgroup creation) up front so the claim path is
// just an activate.
//
// This slice is the OBJECT lifecycle only: the controller creates, scales, and
// owner-ref-GCs husk pod objects. The pods actually running and activating is a
// later kind-e2e slice. The default remains raw-forkd (flag off).

const (
	// huskPoolLabel carries the owning pool name on every husk pod, so a
	// reconcile can list exactly this pool's husk pods.
	huskPoolLabel = "mitos.run/pool"
	// huskLabel marks a pod as a husk pod (vs any other pod the controller may
	// touch). Both labels together form the warm-pool selector.
	huskLabel = "mitos.run/husk"
	// huskMultiVMLabelValue is the value huskMultiVMLabel carries on a pod whose
	// stub runs --multi-vm (defined in sandboxfork_controller.go), so
	// huskPodMultiVMCapable can recognize a co-location-capable source pod.
	huskMultiVMLabelValue = "true"
	// huskContainerName is the single container in a husk pod.
	huskContainerName = "husk-stub"

	// defaultKVMResourceName is the extended resource the KVM device plugin
	// advertises (deploy/device-plugin). A husk pod requests one slot so it is
	// scheduled only onto a node with /dev/kvm; this replaces privileged: true.
	defaultKVMResourceName = "mitos.run/kvm"

	// huskWorkdir is the per-VM working directory the stub uses.
	huskWorkdir = "/run/husk/vm"

	// huskClaimLabel marks a husk pod as claimed by a specific SandboxClaim.
	// Selection skips any pod carrying it: one claim activates one husk pod.
	huskClaimLabel = "mitos.run/claim"

	// huskTemplateDigestAnnotation records the content-addressed snapshot digest a
	// warm husk pod was built to verify against, so the reconcile can reap a pod
	// whose snapshot was rebuilt under it (issue #461). An annotation, not a label:
	// a digest contains ':' and exceeds 63 chars, both invalid in a label value.
	huskTemplateDigestAnnotation = "mitos.run/template-digest"
	// huskSnapshotNodeAnnotation records the single snapshot node a warm husk pod
	// is pinned to, so the reconcile compares the pod's stamped digest against THAT
	// node's current recorded digest (per-node digests differ, issue #175).
	huskSnapshotNodeAnnotation = "mitos.run/snapshot-node"
	// huskBuildGenerationAnnotation records the pool's TemplateBuildGeneration a
	// warm husk pod was created under. After an in-place template rebuild bumps
	// the pool's generation, pods stamped with an older generation (or with no
	// stamp at all, the pre-#679 fallback fleet) reference old artifacts and are
	// reaped, digest or no digest (issue #679).
	huskBuildGenerationAnnotation = "mitos.run/template-build-generation"

	// huskForkVMGuestMemoryAnnotation records ONE fork VM's honest guest-RAM
	// footprint on a MULTI-VM husk pod. A multi-VM pod reserves node memory for the
	// source VM plus a bounded number of co-located fork VMs, so its memory REQUEST
	// is NOT one VM's RAM: this annotation is the per-VM unit coLocatedForkVMBudget
	// divides the reserved request by to recover how many co-located fork VMs the
	// pod reserved room for. A single-VM pod carries no annotation (its request IS
	// one VM's RAM) and the legacy limit-based budget applies unchanged.
	huskForkVMGuestMemoryAnnotation = "mitos.run/fork-vm-guest-memory"

	// huskForkLabel marks a husk pod as a fork CHILD and carries the owning
	// SandboxFork name, so a reconcile can list exactly this fork's children and
	// they are never counted as warm-pool slots (the pool selector requires
	// huskPoolLabel, which fork children do not carry).
	huskForkLabel = "mitos.run/fork"

	// huskKVMNodeLabel is the node label the KVM device plugin / node bootstrap
	// sets on a node that has /dev/kvm (deploy/talos). A husk pod is pinned to
	// such a node so the dormant VMM can open KVM AND so it lands where the
	// template snapshot is materialized (the pool's build/distribution machinery
	// places the snapshot on these nodes; see the placement note below).
	huskKVMNodeLabel = "mitos.run/kvm"

	// HuskControlPort is the fixed TCP port the husk stub serves the mTLS network
	// control on (--control-listen). The controller dials podIP:HuskControlPort
	// to activate. Exported so cmd/controller can pass the same port to the claim
	// reconciler.
	HuskControlPort = 9443

	// huskSandboxPort is the in-pod port the activated VM's sandbox HTTP API is
	// reachable on (exec/files). The claim's Status.Endpoint is podIP:this, the
	// same shape forkd's HTTPEndpoint uses (forkd_discovery defaults 9091).
	huskSandboxPort = 9091

	// In-pod paths the stub's TLS, snapshot, and kernel mounts land on. The
	// snapshot mount is the directory the ActivateRequest.SnapshotDir points at:
	// the stub reads SnapshotDir/mem and SnapshotDir/vmstate (husk/control.go),
	// which is the forkd snapshot subdir <dataDir>/templates/<id>/snapshot. The
	// leaf cert/key and the CA are SEPARATE Secrets (the CA private key must never
	// reach the husk pod), mirroring the forkd DaemonSet's /etc/forkd/tls +
	// /etc/forkd/ca split.
	huskTLSMountPath      = "/etc/husk/tls"
	huskCAMountPath       = "/etc/husk/ca"
	huskSnapshotMountPath = "/var/lib/mitos/snapshot"
	huskKernelMountPath   = "/var/lib/mitos/kernel/vmlinux"
	// huskManifestDirMountPath is the in-pod path the CAS manifests DIRECTORY is
	// mounted at (read-only); the stub reads <dir>/<digest>. The stub decodes
	// that file, binds it to the activate request's ExpectedDigest, re-hashes the
	// loaded snapshot files against it, and runs the snapcompat check, all BEFORE
	// loading the snapshot. This is the husk mirror of forkd's verify-on-load gate
	// (issues #9 and #32). The manifest is a content-addressed artifact, not a
	// secret. We mount the DIRECTORY, not the single manifest file: on Talos the
	// kubelet's single-file hostPath check fails for a file at this depth ("is not
	// a file" / "no such file or directory") even when it exists, while a
	// directory hostPath mounts cleanly and exposes the file inside it.
	huskManifestDirMountPath = "/var/lib/mitos/manifests"
	// huskRootfsCoWMountPath is the in-pod path the writable per-activation rootfs
	// CoW directory is mounted at. It is a hostPath under the node data dir
	// (<dataDir>/husk-rootfs), co-located with the template dir on the SAME node
	// filesystem so the stub's reflink clone of the template rootfs lands on a
	// reflink-capable filesystem (a full copy fallback otherwise). Each activation
	// writes its own clone under here, never the shared read-only template rootfs.
	huskRootfsCoWMountPath = "/var/lib/mitos/husk-rootfs"

	// huskForksMountPath is the in-pod path the node forks DIRECTORY is mounted at
	// (read-write for a source pod that may be forked; the child's read-only
	// snapshot mount points at a subdir of the node forks dir instead). The stub
	// writes <huskForksMountPath>/<fork-id>/{mem,vmstate} on a fork-snapshot op.
	huskForksMountPath = "/var/lib/mitos/forks"

	// huskCASMountPath is the in-pod path the node content-addressed store is
	// mounted at READ-WRITE so the dehydrate-workspace op can persist a captured
	// /workspace into it (and hydrate-workspace can restore from it). It is the
	// same <dataDir>/cas the forkd build path writes, so a workspace revision the
	// husk pod commits is visible to the controller's CAS-backed diff/git path and
	// to a sibling pod that later hydrates the head.
	huskCASMountPath = "/var/lib/mitos/cas"
)

// HuskSnapshotDir is the in-pod path the husk stub treats as ActivateRequest
// .SnapshotDir: the mounted forkd snapshot subdir holding mem and vmstate. The
// claim reconciler threads this into the activate request.
const HuskSnapshotDir = huskSnapshotMountPath

// huskForkRootfsInPodPath returns the IN-POD path a fork child clones its rootfs
// from: the FROZEN source rootfs the source stub captured INSIDE the fork
// snapshot's paused window, written next to mem+vmstate at
// SnapshotDir/rootfs.ext4 and mounted read-only at huskSnapshotMountPath in the
// child. It is a point-in-time copy paired with the memory checkpoint, so the
// child's restored guest memory (page cache, ext4 superblock, in-flight metadata)
// matches the disk exactly. Cloning from the source's LIVE rootfs instead would
// let a resumed source drift the disk out of sync with the checkpoint; cloning
// from the pristine template rootfs would lose every write the source made. The
// frozen copy avoids both.
func huskForkRootfsInPodPath() string {
	return filepath.Join(huskSnapshotMountPath, "rootfs.ext4")
}

// HuskPodOptions configures the husk pod spec the controller emits.
type HuskPodOptions struct {
	// MultiVM starts the husk stub with --multi-vm and labels the pod
	// mitos.run/multi-vm=true, so the pod can host ADDITIONAL fork-child VMs in
	// place (the MultiVMFork co-location routing) instead of every fork getting its
	// own pod. Default off: the stub runs single-VM and huskPodMultiVMCapable
	// reports false, so a fork always spills to a new pod. Enabling it does not
	// change a normal claim: Stub.Activate routes a claim with no VMID to the pod's
	// default VM, byte-for-byte as the single-VM path does.
	MultiVM bool
	// LiveCowFork starts the husk stub with --live-cow-fork so a CO-LOCATED fork
	// child shares the PARENT's resident guest memory (patched Firecracker memfd +
	// userfaultfd write-protect) instead of restoring from the disk fork snapshot
	// (milestone m4b). Default off and SEPARATE from MultiVM so it canaries
	// independently; off keeps the co-located fork on the disk restore byte-for-byte.
	LiveCowFork bool
	// LiveCowChildImport starts the husk stub with --live-cow-child-import so an
	// armed live-cow fork takes the VMSTATE-ONLY capture (skip the ~364ms disk mem
	// write) and the co-located child boots its guest RAM from the source shared
	// memfd. REQUIRES LiveCowFork on and a child-side-import Firecracker binary;
	// default off, fails closed to the disk restore.
	LiveCowChildImport bool
	// PrewarmChild starts the husk stub with --prewarm-child so a multi-vm pod keeps
	// one dormant generic co-located child Firecracker pre-prepared and a fork adopts
	// it (fc_boot off the hot path). DEFAULT OFF; requires MultiVM.
	PrewarmChild bool
	// PrepareEgressLink starts the husk stub with --prepare-egress-link (plus the
	// in-pod link addresses it needs before an activate request arrives), so a warm
	// pod brings its tap up while dormant and a claim pays only the nft transaction
	// that installs the tenant's policy. Requires MultiVM. Default off.
	PrepareEgressLink bool
	// MultiVMForkVMs is the number of ADDITIONAL fork-child VMs a multi-VM pod
	// reserves node memory for up front (beyond the source VM), so the co-location
	// routing has room to co-locate that many children before a fork spills to a new
	// pod. Only consulted when MultiVM is set; zero selects
	// defaultMultiVMForkVMsPerPod. The reservation is on the pod's memory REQUEST so
	// the scheduler keeps it node-honest (guarantee A).
	MultiVMForkVMs int
	// StubImage is the container image that runs cmd/husk-stub.
	StubImage string
	// DNSUpstream is the comma-separated host:port resolver list (failover order)
	// the stub's per-pod DNS proxy forwards allowlisted name queries to. Empty
	// leaves name-based egress off (IP-only allowlists still enforced).
	DNSUpstream string
	// KVMResourceName is the extended resource the husk pod requests for KVM
	// access. Empty defaults to mitos.run/kvm.
	KVMResourceName string
	// SnapshotID names the template snapshot the husk pod activates. It is the
	// template id; the node-local snapshot lives at
	// <DataDir>/templates/<SnapshotID>/snapshot. Empty means no snapshot mount is
	// added (the pod cannot activate; only meaningful with the activation slice).
	SnapshotID string
	// DataDir is the forkd data directory on the node (default /var/lib/mitos).
	// The snapshot hostPath is rooted here. Empty defaults to the forkd default.
	DataDir string
	// ExpectedDigest is the template's recorded CAS manifest digest, as reported
	// by forkd via GetCapacity (the NodeRegistry TemplateDigests). When set, the
	// husk pod mounts the recorded manifest from <DataDir>/cas/manifests/<digest>
	// read-only and runs the stub with verify enforced (--manifest); the stub
	// re-verifies the snapshot against it before loading (fail-closed). Empty means
	// no manifest mount and the stub runs the development escape hatch
	// (--allow-unverified-snapshots) so a pre-digest pool still activates; this is
	// the only non-fail-closed path and is logged loudly by the stub.
	ExpectedDigest string
	// TLSSecretName is the Secret holding the husk stub's mTLS server leaf
	// (tls.crt, tls.key), mounted read-only so the stub can serve the mTLS network
	// control. This mirrors how forkd gets its leaf from a mounted PKI Secret
	// (mitos-forkd-tls). Empty means no TLS mount is added.
	TLSSecretName string
	// CASecretName is the Secret holding the control plane CA (ca.crt only),
	// mounted read-only so the stub can verify the controller client cert. Kept
	// separate from the leaf so the CA private key never reaches the husk pod,
	// mirroring the forkd DaemonSet's /etc/forkd/ca split. Empty means no CA mount.
	CASecretName string
	// ForkSnapshotID, when set, makes this a FORK CHILD pod: it activates from
	// the node fork snapshot <DataDir>/forks/<ForkSnapshotID> instead of the
	// template snapshot. The fork snapshot was written by the source sandbox's
	// husk stub (the fork-snapshot control op). It is a node-local id, not a
	// secret.
	ForkSnapshotID string
	// ForkSourceNode pins a fork child to the node holding the fork snapshot (the
	// source sandbox's node). Required when ForkSnapshotID is set, since the fork
	// snapshot is a node-local hostPath that exists only on that node.
	ForkSourceNode string
	// ForkSourceRootfsPath, set on a FORK CHILD, is the IN-POD path of the FROZEN
	// source rootfs the child's per-activation CoW clone is made from. The source
	// stub captured it inside the fork snapshot's paused window at
	// SnapshotDir/rootfs.ext4, mounted read-only at huskSnapshotMountPath in the
	// child (huskForkRootfsInPodPath). This is load-bearing for fork correctness:
	// the fork snapshot's vmstate was baked against the source's rootfs, so the
	// child's restored guest memory (page cache, ext4 superblock, in-flight
	// metadata) must pair with a disk captured at the SAME instant. The frozen copy
	// is that instant. Cloning from the source's LIVE rootfs would let the resumed
	// source drift the disk out of sync with the checkpoint; cloning from the
	// PRISTINE TEMPLATE rootfs would lose every write the source made. Each child
	// still writes its OWN per-activation clone (independence); only the clone
	// source changes. Empty (a warm pod, not a fork child) clones from the template
	// rootfs.
	ForkSourceRootfsPath string
	// Template, when set on a FORK CHILD, is the resolved SOURCE pool template.
	// buildForkChildPod threads it into buildHuskPod so the child pod carries the
	// SAME per-sandbox resources (cpu burst cap, memory) a warm-claimed sandbox of
	// the source pool gets, instead of the default caps an empty template yields
	// (issue #760). Nil (or a warm pod, which passes its template to buildHuskPod
	// directly) leaves the child on the documented default resources.
	Template *v1.PoolTemplateSpec

	// SnapshotNodes is the set of node hostnames the pool has materialized the
	// template snapshot on (the registry's NodesWithTemplate). When non-empty the
	// husk pod carries a nodeAffinity pinning it to exactly these nodes, so its
	// read-only snapshot hostPath always resolves. PLACEMENT COUPLING: the pool
	// reconcile builds the snapshot on these same nodes before creating husk pods.
	// When empty the pod falls back to the kvm nodeSelector alone (the
	// build-on-all-kvm-nodes coupling: the snapshot is on every kvm node).
	SnapshotNodes []string

	// PlacementNodeSelector and PlacementTolerations come from the pool's
	// spec.placement (dedicatedNodes, issue #172). The selector is MERGED onto the
	// husk pod's KVM nodeSelector (so the VM runs only on the tenant's dedicated
	// nodes); the tolerations are appended so the pod schedules onto tainted
	// dedicated nodes. Both empty/nil leave the pod unconstrained beyond KVM +
	// snapshot-node affinity. For a FORK CHILD, buildForkChildPod fills both from
	// the SOURCE husk pod's own spec (not the pool): the child must land on the
	// source pod's exact node, so the source's scheduling constraints are the
	// authoritative record of what it takes to get there.
	PlacementNodeSelector map[string]string
	PlacementTolerations  []corev1.Toleration
}

// hostnameNodeLabel is the well-known node label carrying the node's hostname.
// A husk pod's nodeAffinity matches it against the snapshot-holding nodes so the
// pod lands only where the template snapshot exists.
const hostnameNodeLabel = "kubernetes.io/hostname"

// defaultDataDir is the forkd data directory default; the snapshot hostPath is
// rooted here when HuskPodOptions.DataDir is empty (matches cmd/forkd's
// --data-dir default).
const defaultDataDir = "/var/lib/mitos"

// defaultHuskCPU is the CPU burst cap (Limits[cpu]) used when the template
// carries no Resources.CPU. It is the configured per-sandbox CPU ceiling: the
// kubelet enforces cgroup cpu.max at this value. The matching Requests[cpu] is
// defaultHuskCPURequestFloor (50m), intentionally much lower so many idle warm
// husks pack onto one node (the scheduler packs by request); each VM can then
// burst up to its cpu limit once active. Agent sandboxes spend most time waiting
// on model replies, so the request reflects the dormant footprint while the limit
// bounds burst (a CPU DoS cap per sandbox, new with this model). The k8s CPU
// request is decoupled from the guest vCPU count (firecracker.VMConfig.VcpuCount).
//
// defaultHuskCPURequestFloor is the low floor applied to Requests[cpu]: the
// scheduler places pods by sum-of-requests, so a small floor lets idle warm husks
// pack densely. The floor is capped to the configured limit (request never exceeds
// limit). Operators can raise the template cpu to widen the burst cap.
var (
	defaultHuskCPU             = resource.MustParse("250m")
	defaultHuskCPURequestFloor = resource.MustParse("50m")
	defaultHuskMemory          = resource.MustParse("512Mi")
)

// Memory-limit headroom defaults (production-blocker #2, cap 1). The husk
// container's memory LIMIT is sized = memory request + headroom, where the
// headroom is max(defaultHuskMemoryHeadroom, defaultHuskMemoryHeadroomPercent%
// of the request). The headroom exists because the cgroup the limit caps holds
// MORE than the guest's configured RAM: the Firecracker VMM, the husk-stub, and
// copy-on-write dirty-page slack as the restored VM faults in and writes pages.
// A limit equal to the request would OOM-kill a VM running normally at its
// configured RAM and destroy the activate latency; the headroom is what keeps
// the limit transparent to a legitimate VM while still capping a runaway tenant.
// Both are operator-tunable via the controller flags.
var defaultHuskMemoryHeadroom = resource.MustParse("256Mi")

const defaultHuskMemoryHeadroomPercent = 25

// defaultMultiVMForkVMsPerPod is the number of ADDITIONAL fork-child VMs a
// multi-VM husk pod reserves node memory for up front (beyond the source VM), so
// the MultiVMFork co-location routing has room to co-locate that many fork children
// in place before a fork spills to a new pod. It is the reserved co-location count
// a multi-VM pod is sized for; the reservation is on the pod's memory REQUEST, so
// the scheduler places the pod only where every co-located VM's guest RAM
// physically fits (guarantee A: a co-located fork never overcommits the node). It
// restores the co-location capacity the earlier hardcoded per-pod count granted
// before the memory-budget accounting sized every realistic pod down to zero.
const defaultMultiVMForkVMsPerPod = 4

// huskMemoryLimit returns the husk container's memory limit: the memory request
// plus the headroom max(floor, percent% of the request). It never returns a
// value less than or equal to the request, so a VM at its configured RAM is
// never OOM-killed by a too-tight limit. floor zero selects the default floor;
// percent zero selects the default percent.
func huskMemoryLimit(memReq, floor resource.Quantity, percent int) resource.Quantity {
	if floor.IsZero() {
		floor = defaultHuskMemoryHeadroom
	}
	if percent <= 0 {
		percent = defaultHuskMemoryHeadroomPercent
	}
	// Proportional component: percent% of the request, computed in bytes to
	// avoid losing precision on binary-SI quantities.
	proportional := resource.NewQuantity(memReq.Value()*int64(percent)/100, memReq.Format)
	headroom := floor
	if proportional.Cmp(headroom) > 0 {
		headroom = *proportional
	}
	limit := memReq.DeepCopy()
	limit.Add(headroom)
	return limit
}

// scaleQuantity returns q multiplied by n, preserving q's binary-SI format so a
// scaled memory reservation still prints in Mi/Gi. Used to reserve a multi-VM husk
// pod's memory for (1 + reserved fork VMs) guest-RAM footprints.
func scaleQuantity(q resource.Quantity, n int64) resource.Quantity {
	return *resource.NewQuantity(q.Value()*n, q.Format)
}

// buildHuskPod builds the warm-pool husk pod for a pool. The pod is
// huskPodLabels builds a husk pod's label set: the warm-pool selector
// (huskPoolLabel + huskLabel), plus huskMultiVMLabel when the stub runs
// --multi-vm so huskPodMultiVMCapable recognizes the pod as a co-location source.
func huskPodLabels(poolName string, multiVM bool) map[string]string {
	labels := map[string]string{
		huskPoolLabel: poolName,
		huskLabel:     "true",
	}
	if multiVM {
		labels[huskMultiVMLabel] = huskMultiVMLabelValue
	}
	return labels
}

// GenerateName <pool>-husk- in the pool namespace, owner-referenced to the pool
// for garbage collection, labeled for the warm-pool selector, and runs the
// dormant stub with a non-privileged securityContext.
func (r *SandboxPoolReconciler) buildHuskPod(pool *v1.SandboxPool, template *v1.PoolTemplateSpec, opts HuskPodOptions) *corev1.Pod {
	kvmResource := opts.KVMResourceName
	if kvmResource == "" {
		kvmResource = defaultKVMResourceName
	}

	// CPU: limit = configured per-sandbox cap (Limits[cpu] = cgroup cpu.max).
	// Request = low floor so idle warm husks pack densely onto nodes; the scheduler
	// places pods by sum-of-requests, so a small request (50m) lets many dormant
	// husks share a node while each VM bursts to its limit when active. The floor is
	// capped to the limit so request never exceeds limit (a k8s requirement for
	// Burstable QoS). This is the OVERCOMMIT LEVER: operators declare the cap via the
	// pool template; the floor is an internal density knob, not operator-visible.
	cpuLimit := defaultHuskCPU
	if !template.Resources.CPU.IsZero() {
		cpuLimit = template.Resources.CPU
	}
	cpuFloor := defaultHuskCPURequestFloor
	if cpuFloor.Cmp(cpuLimit) > 0 {
		cpuFloor = cpuLimit
	}
	// perVMGuestMem is ONE VM's honest guest-RAM footprint (the tenant's configured
	// sandbox memory, or the default). It is what a co-located fork VM costs at the
	// CoW worst case and the per-VM unit coLocatedForkVMBudget divides a multi-VM
	// pod's reserved memory by.
	perVMGuestMem := defaultHuskMemory
	if !template.Resources.Memory.IsZero() {
		perVMGuestMem = template.Resources.Memory
	}
	// Memory REQUEST. A single-VM pod requests one VM's guest RAM (honest, no
	// overcommit: Firecracker holds the guest RAM resident in the pod cgroup). A
	// MULTI-VM pod exists to host ADDITIONAL fork-child VMs co-located in place (the
	// MultiVMFork routing), so it RESERVES memory up front for the source VM plus a
	// bounded number of co-located fork VMs (the plan's "reserve the pod at a bounded
	// max VM count up front"). Reserving on the REQUEST keeps the reservation
	// node-honest: the scheduler places the pod only where every co-located VM's
	// guest RAM physically fits, so a co-located fork never overcommits the node
	// (guarantee A). Without this reservation a multi-VM pod was sized for ONE VM,
	// its co-location budget floored to 0, every fork spilled to a new pod, and the
	// spill path is exactly where the production canary failed
	// (re-get-fork-child-pod-not-found). Memory is non-compressible and CPU is not,
	// so only memory is reserved this way; CPU stays burstable.
	memReq := perVMGuestMem
	if opts.MultiVM {
		reservedForkVMs := opts.MultiVMForkVMs
		if reservedForkVMs <= 0 {
			reservedForkVMs = defaultMultiVMForkVMsPerPod
		}
		memReq = scaleQuantity(perVMGuestMem, int64(1+reservedForkVMs))
	}
	// Memory LIMIT = request + headroom (production-blocker #2, cap 1). The
	// headroom (floor + proportional) is operator-tunable on the reconciler; the
	// zero values select the documented defaults (256Mi floor, 25 percent).
	memLimit := huskMemoryLimit(memReq, r.HuskMemoryHeadroom, r.HuskMemoryHeadroomPercent)

	// SecurityContext decisions (each load-bearing; the husk pod is the new
	// execution surface, so it is locked down and the device exception is the KVM
	// device plugin, NOT privileged).
	//
	// PSA AUDIT (empirically verified against the v1.31 PodSecurity admission
	// plugin on kind, proven object-level in the kind-e2e conformance job): the
	// husk pod's securityContext satisfies EVERY restricted control, but the husk
	// pod is NOT admitted into a baseline or restricted namespace, for exactly two
	// DOCUMENTED EXCEPTIONS, both intrinsic to the husk model:
	//   1. the read-only node hostPaths. hostPath is forbidden under BOTH baseline
	//      and restricted (the "HostPath Volumes" / "Volume Types" controls); the
	//      husk pod mounts the node's read-only template snapshot (mem+vmstate) so
	//      the dormant VMM can load it, the guest kernel, and (when the pool has a
	//      recorded digest) the read-only CAS manifest the stub verifies the
	//      snapshot against before loading. These are all the same node-hostPath
	//      exception category (read-only, intrinsic to the node-local snapshot
	//      model); none is writable.
	//   2. runAsNonRoot=false. restricted requires runAsNonRoot=true; the husk pod
	//      runs uid 0 so Firecracker can open the device-plugin-injected /dev/kvm
	//      WITHOUT privileged (the /dev/kvm device exception).
	// So the HONEST claim is: the husk pod is "restricted EXCEPT the read-only
	// snapshot hostPath + runAsNonRoot-false (the /dev/kvm device) exceptions". Its
	// securityContext is restricted-clean: with those two exceptions removed the
	// SAME securityContext is admitted into a restricted namespace (verified on
	// kind). The mitos.run/kvm device-plugin resource replaces privileged: true.
	//
	// The individual controls the husk pod DOES satisfy:
	//   - Privileged: false. The whole point of the husk model is to drop
	//     privileged: true; KVM access comes from the device plugin slot, not
	//     from a privileged container.
	//   - AllowPrivilegeEscalation: false. No setuid path can regain privilege.
	//   - Capabilities Drop ALL, add NET_ADMIN. The dormant stub Prepares a
	//     Firecracker VMM (open /dev/kvm via the device plugin, create files
	//     under the pod-local workdir, bind a unix socket); none of that needs a
	//     Linux capability. NET_ADMIN is added because the stub programs the
	//     in-pod nftables egress filter and the VM's tap in the pod's OWN network
	//     namespace at activation (the load-bearing isolation control). It is the
	//     minimal capability for that and is scoped to the pod netns: the pod is
	//     not hostNetwork and not privileged, so it cannot reach the host netns,
	//     another pod's netns, or the node routing tables. Recorded as a PSA
	//     exception in docs/threat-model.md.
	//   - SeccompProfile RuntimeDefault, set at BOTH the pod and the container
	//     securityContext level. restricted checks the profile at the pod OR the
	//     container level; setting both keeps the pod-level control satisfied even
	//     if a future container is added without its own profile.
	//   - RunAsNonRoot: false (the documented /dev/kvm device exception above),
	//     set at both the pod and the container level. A follow-up slice can move
	//     to a non-root uid in the kvm group once the device plugin's device
	//     permissions are pinned. It is NOT privileged and escalation is denied.
	runAsNonRoot := false

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir
	}

	// The stub args. The husk pod serves the mTLS NETWORK control on
	// HuskControlPort (not the unix --control-socket the in-CI driver uses): the
	// controller dials podIP:HuskControlPort to activate. The three TLS PEM paths
	// point at the mounted PKI Secret (mirrors how forkd reads its leaf + CA from
	// a mounted Secret). The kernel and snapshot are read-only mounts below.
	args := []string{
		"--firecracker", "/usr/local/bin/firecracker",
		"--kernel", huskKernelMountPath,
		"--workdir", huskWorkdir,
		"--control-listen", fmt.Sprintf(":%d", HuskControlPort),
		// Serve the in-pod sandbox HTTP API (exec/files) on the declared sandbox
		// container port after activation, gated by the per-sandbox bearer token
		// delivered over the control channel. The claim's Status.Endpoint is
		// podIP:huskSandboxPort, so the stub must serve there.
		"--sandbox-listen", fmt.Sprintf(":%d", huskSandboxPort),
		"--tls-cert", filepath.Join(huskTLSMountPath, "tls.crt"),
		"--tls-key", filepath.Join(huskTLSMountPath, "tls.key"),
		"--tls-ca", filepath.Join(huskCAMountPath, "ca.crt"),
		// Per-pod VM id from the downward API pod name (set on the container env
		// below). It scopes this pod's per-activation rootfs CoW clone path
		// (<rootfs-cow-dir>/<id>/rootfs.ext4), which is written under a node
		// hostPath shared by EVERY husk pod on the node. Without a per-pod id every
		// husk pod would clone to the IDENTICAL path and overwrite or delete each
		// other's live rootfs (cross-pod corruption); the pod name is unique per
		// node, so each pod gets its own clone. $(POD_NAME) is substituted by the
		// kubelet from the env var, not the shell.
		"--vm-id", "$(POD_NAME)",
	}

	// Multi-VM mode: the stub accepts spawn-vm ops to host ADDITIONAL fork-child
	// VMs in this pod (the MultiVMFork co-location routing). Default off leaves the
	// stub single-VM. A normal claim is unaffected either way (Activate routes a
	// VMID-less claim to the pod's default VM).
	if opts.MultiVM {
		args = append(args, "--multi-vm")
	}
	// Live-cow fork (milestone m4b), default off and separate from --multi-vm: a
	// co-located fork child shares the parent's resident guest memory instead of
	// restoring from the disk fork snapshot. Off keeps the disk co-location
	// byte-for-byte; on it fails closed to the disk restore where the child-side
	// import is not yet complete, so it never breaks a fork.
	if opts.LiveCowFork {
		args = append(args, "--live-cow-fork")
	}
	if opts.LiveCowChildImport {
		args = append(args, "--live-cow-child-import")
	}
	if opts.PrewarmChild {
		args = append(args, "--prewarm-child")
	}
	if opts.PrepareEgressLink {
		// The stub needs the in-pod link BEFORE a claim arrives, because the tap name
		// derives from the guest IP. These are the same fixed values huskNotifyNetwork
		// sends in the activate request; they are config, not secrets.
		args = append(args,
			"--prepare-egress-link",
			"--in-pod-guest-ip", huskGuestIP,
			"--in-pod-gateway-ip", huskGatewayIP,
		)
	}

	// Live-cow write-protect needs a KERNEL-MODE userfaultfd over the guest RAM: the
	// source Firecracker registers UFFD_WP so the KVM guest's own writes fault to the
	// copy-before-unprotect handler (issue #832). The patched restore path creates
	// that userfaultfd via the `/dev/userfaultfd` DEVICE (open + USERFAULTFD_IOC_NEW),
	// NOT the `userfaultfd(2)` syscall: the container RuntimeDefault seccomp profile
	// denies `userfaultfd(2)` with EPERM even when CAP_SYS_PTRACE is present, because
	// CAP_SYS_PTRACE satisfies only the kernel gate, not the seccomp gate. The device
	// is injected into every KVM husk pod by the kvm device plugin (mitos.run/kvm),
	// which also sets the device-cgroup allow a plain hostPath cannot, and the ioctl
	// device path is permitted by the same seccomp profile. So the husk-stub keeps the
	// minimal NET_ADMIN-only capability set: no CAP_SYS_PTRACE is needed on a live-cow
	// pool. Documented in docs/threat-model.md.
	huskCaps := []corev1.Capability{"NET_ADMIN"}

	// Name-based egress: when the operator configured DNS upstream(s), pass them
	// to the stub so the per-pod DNS proxy resolves and pins allowlisted names.
	// Empty leaves name-based egress off (IP-only allowlists still work); the
	// value is a comma-separated host:port failover list, config not a secret.
	if opts.DNSUpstream != "" {
		args = append(args, "--dns-upstream", opts.DNSUpstream)
	}

	// Snapshot verify gate (fail-closed): when the pool has a recorded template
	// digest, mount the recorded CAS manifest and point the stub at it so it
	// re-verifies the snapshot (digest + snapcompat) before loading. Without a
	// recorded digest (a pool whose snapshot has not been content-addressed yet)
	// fall back to the development escape hatch so the warm pool still activates;
	// the stub logs this loudly. The manifest mount itself is added in the snapshot
	// block below (it shares the snapshot placement requirement).
	if opts.ForkSnapshotID != "" {
		// Fork child: the fork snapshot is a LIVE, node-local artifact created by
		// the source stub and consumed by child stubs on the SAME node within the
		// same trust boundary. It is NOT content-addressed (re-hashing would gate
		// on a digest that does not exist for a live fork), so the child activates
		// it with verify disabled, the same posture a pre-digest pool uses. The
		// child still runs the full fail-closed RNG/clock reseed handshake.
		args = append(args, "--allow-unverified-snapshots")
	} else if opts.ExpectedDigest != "" {
		args = append(args, "--manifest", filepath.Join(huskManifestDirMountPath, opts.ExpectedDigest))
		// Pass the snapshot dir + expected digest so the dormant pod verifies the
		// snapshot (the ~680 MiB re-hash) during Prepare, off the claim's Activate
		// hot path. The claim then activates in ~tens of ms (load + handshake)
		// instead of ~1.3 s (re-hash). The activate request carries the same
		// SnapshotDir + ExpectedDigest, which the stub confirms before loading.
		args = append(args, "--snapshot-dir", huskSnapshotMountPath, "--expected-digest", opts.ExpectedDigest)
	} else {
		args = append(args, "--allow-unverified-snapshots")
	}

	// Volumes + mounts: the mTLS Secret, the node's template snapshot subdir
	// (read-only hostPath; the stub reads SnapshotDir/{mem,vmstate}), and the
	// guest kernel. PLACEMENT REQUIREMENT: the snapshot hostPath assumes the
	// template snapshot is materialized on this pod's node. The pod is pinned to a
	// KVM node (nodeSelector below); the pool's existing snapshot
	// build/distribution machinery must ensure the snapshot is present on those
	// nodes. A refinement (CAS-pull the snapshot into the pod) removes the
	// hostPath dependency; documented as a follow-up.
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	if opts.TLSSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "husk-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: opts.TLSSecretName},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-tls", MountPath: huskTLSMountPath, ReadOnly: true})
	}
	if opts.CASecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "husk-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: opts.CASecretName,
					// Only the CA certificate is projected; the CA private key in
					// this Secret must never reach the husk pod.
					Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-ca", MountPath: huskCAMountPath, ReadOnly: true})
	}
	if opts.SnapshotID != "" {
		// DirectoryOrCreate / FileOrCreate, not the strict Directory / File: on
		// Talos the kubelet's strict hostPath type check rejects these mounts
		// ("is not a file/directory") even when the path exists and is the right
		// type, because the kubelet performs the os.Stat in a mount view that
		// differs from where the pod bind mount resolves. The OrCreate variants
		// skip that pre-check and bind the existing snapshot/kernel/manifest. The
		// safety this drops (fail-fast if the snapshot is missing) is not the real
		// gate anyway: the husk stub re-verifies the snapshot against the recorded
		// CAS manifest digest before loading (fail-closed), so an empty or wrong
		// snapshot is rejected at activation, not silently run.
		hostType := corev1.HostPathDirectoryOrCreate

		// The node forks dir, mounted READ-WRITE so this pod's stub can write a
		// fork snapshot of its running VM (<forks>/<fork-id>/{mem,vmstate}) when
		// the controller drives a fork-snapshot op against it. Co-located with the
		// template dir on the SAME node filesystem so a child's read-only mount of
		// a subdir resolves and any CoW stays on one filesystem.
		volumes = append(volumes, corev1.Volume{
			Name: "husk-forks",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "forks"),
					Type: &hostType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-forks", MountPath: huskForksMountPath})
		// Tell the stub where to write fork snapshots so a fork-snapshot op writes
		// inside the mounted node forks dir.
		args = append(args, "--forks-dir", huskForksMountPath)

		// The node CAS, mounted READ-WRITE so this pod's stub can persist a captured
		// /workspace (dehydrate-workspace op) into the content-addressed store and
		// restore a head from it (hydrate-workspace op). Co-located with the template
		// dir on the SAME node filesystem (<dataDir>/cas) so a revision the husk pod
		// commits is the SAME store the controller's CAS-backed diff/git path and a
		// sibling pod read. A workspace revision is content only; secrets are excluded
		// by the dehydrate op's exclude list.
		volumes = append(volumes, corev1.Volume{
			Name: "husk-cas",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "cas"),
					Type: &hostType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-cas", MountPath: huskCASMountPath})
		// Tell the stub where the node CAS is so the workspace ops persist/restore
		// there; empty would disable them (fail-closed).
		args = append(args, "--cas-dir", huskCASMountPath)

		// The snapshot SOURCE path. A warm pod activates from the template's
		// snapshot subdir; a FORK CHILD activates from the node fork snapshot
		// <dataDir>/forks/<fork-id> instead (mounted at the SAME in-pod path).
		snapshotHostPath := filepath.Join(dataDir, "templates", opts.SnapshotID, "snapshot")
		if opts.ForkSnapshotID != "" {
			snapshotHostPath = filepath.Join(dataDir, "forks", opts.ForkSnapshotID)
		}
		volumes = append(volumes, corev1.Volume{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: snapshotHostPath,
					Type: &hostType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "snapshot", MountPath: huskSnapshotMountPath, ReadOnly: true})

		fileType := corev1.HostPathFileOrCreate

		// The recorded CAS manifest, mounted read-only so the stub can re-verify
		// the snapshot against it before loading (fail-closed). Only added when the
		// pool has a recorded digest; the file lives at
		// <dataDir>/cas/manifests/<digest> on the same node the snapshot is on.
		if opts.ExpectedDigest != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "snapshot-manifest",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: filepath.Join(dataDir, "cas", "manifests"),
						Type: &hostType,
					},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{Name: "snapshot-manifest", MountPath: huskManifestDirMountPath, ReadOnly: true})
		}
		volumes = append(volumes, corev1.Volume{
			Name: "kernel",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "vmlinux"),
					Type: &fileType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "kernel", MountPath: huskKernelMountPath, ReadOnly: true})

		// The template directory, mounted at the SAME absolute path the snapshot
		// was built at (<dataDir>/templates/<id>), so the rootfs.ext4 drive the
		// snapshot's vmstate references resolves on load. Firecracker re-opens the
		// drive at its baked path_on_host during /snapshot/load; without this the
		// load fails with "Block: Virtio backend error" (the drive file is absent
		// in the husk pod's mount namespace). A directory mount, not a single-file
		// one, both sidesteps the Talos single-file hostPath check and exposes the
		// rootfs at exactly the baked path.
		//
		// The stub then rebinds the rootfs drive to a PER-ACTIVATION copy-on-write
		// clone (see the husk-rootfs-cow mount below) immediately after load, so the
		// resumed VM writes its OWN rootfs, never the shared template rootfs:
		// concurrent activations of one template no longer share or corrupt a single
		// rootfs. The clone source (this template rootfs) stays effectively read-only.
		templateDir := filepath.Join(dataDir, "templates", opts.SnapshotID)
		volumes = append(volumes, corev1.Volume{
			Name: "template",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: templateDir,
					Type: &hostType,
				},
			},
		})
		// Mounted READ-WRITE, but the guest never writes the template. Firecracker
		// opens the snapshot's BAKED rootfs path (this template rootfs.ext4) with
		// O_RDWR during /snapshot/load, so a read-only mount makes the load fail
		// EROFS (verified on real KVM). The VM stays PAUSED through load (resume
		// =false) -> PatchDrive(rootfs -> per-pod clone) -> Resume, so by the time
		// the guest runs its rootfs is the per-activation CoW clone (the SEPARATE
		// writable husk-rootfs mount below), never the template. Isolation is from
		// the rebind-before-resume, not the mount mode: the template is only OPENED
		// (not written) during the paused load and the fd is replaced by PatchDrive
		// before resume, so concurrent activations never write the shared template.
		mounts = append(mounts, corev1.VolumeMount{Name: "template", MountPath: templateDir})

		// The writable per-activation rootfs CoW directory, a sibling of the
		// template dir under the node data dir so the stub's reflink clone of the
		// template rootfs stays on ONE reflink-capable filesystem. Mounted
		// READ-WRITE (unlike the snapshot and template mounts) because the stub
		// writes this activation's clone here; an emptyDir would land on a
		// different filesystem and defeat reflink. DirectoryOrCreate so the dir is
		// created on first use.
		volumes = append(volumes, corev1.Volume{
			Name: "husk-rootfs-cow",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: filepath.Join(dataDir, "husk-rootfs"),
					Type: &hostType,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "husk-rootfs-cow", MountPath: huskRootfsCoWMountPath})

		// The clone SOURCE for this pod's per-activation rootfs CoW. A WARM pod
		// clones from the template rootfs (a fresh boot-time disk). A FORK CHILD
		// must clone from the FROZEN source rootfs the source stub captured inside
		// the fork snapshot's paused window instead: the fork snapshot's vmstate was
		// baked against the source's rootfs at that instant, so the child's restored
		// guest memory pairs with that exact disk; cloning from the template would
		// rebind the child to a disk that does not match its memory (silent data
		// divergence / fs corruption). ForkSourceRootfsPath is the in-pod path of
		// the frozen rootfs (SnapshotDir/rootfs.ext4, on the read-only snapshot
		// mount the child also restores mem+vmstate from).
		cloneSourceRootfs := filepath.Join(templateDir, "rootfs.ext4")
		if opts.ForkSourceRootfsPath != "" {
			cloneSourceRootfs = opts.ForkSourceRootfsPath
		}
		// Tell the stub where to clone from (the clone source rootfs at its in-pod
		// path) and to (the writable CoW dir). At Prepare the stub reflink-clones the
		// source rootfs to a per-pod file under the CoW dir and at Activate rebinds
		// the rootfs drive to it, so each activation gets its OWN rootfs clone.
		args = append(args,
			"--rootfs-cow-dir", huskRootfsCoWMountPath,
			"--template-rootfs", cloneSourceRootfs,
		)
	}

	// Placement: the dormant VMM needs /dev/kvm (the kvm nodeSelector) AND the
	// read-only snapshot hostPath must resolve, so the pod must land where the
	// pool materialized the template snapshot. When the pool passes the
	// snapshot-holding node hostnames (NodesWithTemplate), a required nodeAffinity
	// pins the pod to exactly those nodes; without it, the pod falls back to the
	// kvm nodeSelector alone (the snapshot is then assumed present on every kvm
	// node, the documented build-on-all-kvm-nodes coupling).
	var affinity *corev1.Affinity
	if len(opts.SnapshotNodes) > 0 {
		nodes := append([]string(nil), opts.SnapshotNodes...)
		sort.Strings(nodes)
		affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      hostnameNodeLabel,
							Operator: corev1.NodeSelectorOpIn,
							Values:   nodes,
						}},
					}},
				},
			},
		}
	}
	// A fork child can only run where its fork snapshot exists (the source node's
	// node-local hostPath), so pin it to exactly that node. This takes precedence
	// over SnapshotNodes; a fork child never sets SnapshotNodes.
	if opts.ForkSourceNode != "" {
		affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      hostnameNodeLabel,
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{opts.ForkSourceNode},
						}},
					}},
				},
			},
		}
	}

	// Stamp the digest on EVERY warm pod created while a digest is known, so the
	// stale-digest reap (issue #461) covers all creation paths, not only the
	// single-pinned-node one (issue #679). The node annotation is only stamped
	// when the pod is pinned to exactly one snapshot node; otherwise the reap
	// falls back to spec.nodeName once the scheduler has placed the pod. The
	// build generation is stamped unconditionally: it is the rebuild-reap signal
	// for pods created when NO digest was known anywhere (the fallback fleet
	// that hit prod in #679). Fork children never pass through this builder's
	// pool scale-up caller with a stale-generation risk: they activate from a
	// fork snapshot, not the template, and carry no huskPoolLabel, so the pool
	// reap never sees them.
	annotations := map[string]string{
		huskBuildGenerationAnnotation: strconv.FormatInt(pool.Status.TemplateBuildGeneration, 10),
	}
	if opts.ExpectedDigest != "" {
		annotations[huskTemplateDigestAnnotation] = opts.ExpectedDigest
	}
	if len(opts.SnapshotNodes) == 1 && opts.ExpectedDigest != "" {
		annotations[huskSnapshotNodeAnnotation] = opts.SnapshotNodes[0]
	}
	// Record ONE fork VM's guest RAM on a multi-VM pod: its memory request reserves
	// room for MANY VMs, so coLocatedForkVMBudget needs this per-VM unit to recover
	// how many co-located fork VMs the pod reserved room for. A single-VM pod carries
	// no stamp and keeps the legacy limit-based budget.
	if opts.MultiVM {
		annotations[huskForkVMGuestMemoryAnnotation] = perVMGuestMem.String()
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-husk-",
			Namespace:    pool.Namespace,
			Labels:       huskPodLabels(pool.Name, opts.MultiVM),
			Annotations:  annotations,
		},
		Spec: corev1.PodSpec{
			// A husk pod is long-lived: it holds its dormant (then activated) VM
			// until terminated. Restart on crash so the warm slot recovers.
			RestartPolicy: corev1.RestartPolicyAlways,
			// Do NOT automount the namespace default ServiceAccount token. The
			// husk stub speaks vsock + mTLS and never calls the Kubernetes API
			// (no client-go, no InClusterConfig, no SA token read anywhere in
			// cmd/husk-stub or internal/husk), so the token is dead weight. Worse,
			// a guest that escapes into the stub would otherwise inherit a free
			// system:authenticated token (and whatever the default SA can do in
			// the pool namespace). Opting out closes that surface. Applies to BOTH
			// warm pods and fork-child pods, which share this builder.
			AutomountServiceAccountToken: ptrBool(false),
			// POD-LEVEL securityContext. PSA restricted checks seccompProfile and
			// runAsNonRoot at the pod OR the container level; we set them at the pod
			// level too so the pod-level control is satisfied independently of any
			// container. seccompProfile is RuntimeDefault (a restricted control the
			// husk pod satisfies); runAsNonRoot mirrors the documented /dev/kvm
			// device exception (false). The two PSA exceptions that keep the husk
			// pod out of a restricted namespace are the read-only snapshot hostPath
			// and this runAsNonRoot=false, both documented above.
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrBool(runAsNonRoot),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			// Pin to a KVM node: the dormant VMM needs /dev/kvm AND the pod must
			// land where the template snapshot hostPath exists. The nodeAffinity
			// above narrows further to the snapshot-holding nodes when known. The
			// pool's spec.placement nodeSelector (dedicatedNodes, #172) is MERGED in
			// so the VM runs only on the tenant's dedicated nodes.
			NodeSelector: mergedHuskNodeSelector(opts),
			Affinity:     affinity,
			// Evict a husk pod from a NotReady/unreachable node well before the
			// Kubernetes default 300s, so node-loss FAILOVER (re-pend of an active
			// claim onto a surviving node via checkHuskPodLost) and warm-pool refill
			// happen in ~a minute instead of ~5 (#177). A transient reboot is still
			// ridden out: the node returns within the node-monitor grace + this
			// window, so the pod is not evicted and the readiness reflection
			// (reflectHuskBackingReadiness) covers the brief unreachable window.
			// The pool's spec.placement tolerations (#172) are appended so the pod
			// schedules onto tainted dedicated nodes.
			Tolerations: huskTolerations(opts),
			Volumes:     volumes,
			Containers: []corev1.Container{
				{
					Name:  huskContainerName,
					Image: opts.StubImage,
					// Prepare a dormant Firecracker VMM and serve the mTLS network
					// control. The firecracker binary is provided by the image
					// (see Dockerfile.husk-stub); the guest kernel and the template
					// snapshot are read-only hostPath mounts. The controller dials
					// the control port to activate (slice 2).
					Args: args,
					// POD_NAME via the downward API: the kubelet substitutes it
					// into the --vm-id $(POD_NAME) arg above so each husk pod gets
					// a UNIQUE per-pod VM id. That id scopes the per-activation
					// rootfs CoW clone path under the shared node hostPath, so two
					// husk pods on one node never collide on, overwrite, or delete
					// each other's rootfs clone. The pod name is unique per node.
					Env: []corev1.EnvVar{{
						Name: "POD_NAME",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.name",
							},
						},
					}},
					Ports: []corev1.ContainerPort{{
						// The activated VM's sandbox HTTP API (exec/files). The
						// claim's Status.Endpoint is podIP:this, so it must be a
						// declared container port to be reachable.
						Name:          "sandbox",
						ContainerPort: huskSandboxPort,
						Protocol:      corev1.ProtocolTCP,
					}},
					// Readiness gates on the dormant control listener (:9443), which
					// the stub serves only AFTER it reaches StateDormant (Prepare
					// done: per-activation rootfs cloned, Firecracker VMM prepared).
					// Without it the pod reports Ready the instant the container
					// starts, so the pool counts it warm and a claim activates it
					// before the stub is dormant-serving; that activate fails, the pod
					// is consumed, and the warm pool churns (over-creates). The probe
					// has no liveness counterpart, so a not-yet-dormant pod is held
					// out of the pool but never restarted by it; the bounded rootfs
					// wait inside Prepare governs the genuine startup ceiling.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{
								Port: intstr.FromInt(HuskControlPort),
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       3,
						TimeoutSeconds:      2,
						FailureThreshold:    6,
					},
					VolumeMounts: mounts,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							// KVM: request == limit == 1 (device-plugin semantics).
							corev1.ResourceName(kvmResource): resource.MustParse("1"),
							// CPU REQUEST: the low floor (default 50m) is the
							// overcommit lever. The scheduler places pods by
							// sum-of-requests, so a small request lets many idle
							// warm husks pack densely onto one node. QoS stays
							// Burstable (requests < limits). Request never exceeds
							// the limit (capped to cpuLimit before this block).
							corev1.ResourceCPU: cpuFloor,
							// Memory: honest request (no overcommit). Firecracker
							// holds the guest RAM as genuinely resident pages in
							// this cgroup; overcommitting memory would OOM-kill
							// live sandboxes under node pressure. CPU is burstable
							// and safe to overcommit; memory is not.
							corev1.ResourceMemory: memReq,
						},
						Limits: corev1.ResourceList{
							// The KVM device is a countable device-plugin
							// resource: request and limit must be equal and
							// non-zero.
							corev1.ResourceName(kvmResource): resource.MustParse("1"),
							// CPU LIMIT: the configured per-sandbox burst cap,
							// enforced as cgroup cpu.max by the kubelet. Users
							// declare this cap via pool spec.template.resources.cpu;
							// the low request (cpuFloor) enables overcommit while
							// this limit bounds the burst of any one sandbox. This
							// replaces the prior requests-only model: restore and
							// activate now run up to this cap (a deliberate
							// utilization-for-bounded-burst tradeoff; the cap is
							// set generously by operators so a dormant-to-active
							// transition is not throttled below the configured
							// ceiling). QoS is Burstable, not BestEffort, because
							// both cpu and memory limits are set.
							corev1.ResourceCPU: cpuLimit,
							// Memory LIMIT (production-blocker #2, cap 1): the
							// host-DoS cap. Sized = request + headroom so a VM
							// running normally at its configured RAM is NEVER
							// OOM-killed (the headroom covers the Firecracker
							// VMM, the husk-stub, and CoW dirty-page slack),
							// while a runaway tenant is capped before it can OOM
							// the node. This is an O(1) SIZING decision at pod
							// CREATE, off the activate/fork hot path: the kubelet
							// enforces the cgroup limit, the controller never
							// throttles the running VM. A too-tight limit (limit
							// == request) would OOM-kill the VM and destroy the
							// activate latency, which is why the headroom is
							// load-bearing and operator-tunable.
							corev1.ResourceMemory: memLimit,
						},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged:               ptrBool(false),
						AllowPrivilegeEscalation: ptrBool(false),
						RunAsNonRoot:             ptrBool(runAsNonRoot),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
							// NET_ADMIN is the MINIMAL capability for in-pod
							// firewalling: the husk-stub programs nftables and the
							// VM's tap in the pod's OWN network namespace (not the
							// host's). The pod is not hostNetwork and not
							// privileged, so NET_ADMIN cannot reach the host netns,
							// another pod's netns, or the node routing tables. It is
							// the load-bearing control that gives the husk guest VM
							// CNI-independent default-deny egress + the unconditional
							// cloud-metadata block. Documented as a PSA exception in
							// docs/threat-model.md. SYS_PTRACE is appended only for a
							// live-cow pool (see huskCaps above), for the kernel-mode
							// userfaultfd the write-protect fork needs.
							Add: huskCaps,
						},
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			},
		},
	}

	// Billing trust boundary (issue #164): stamp the metering attribution org on
	// the husk pod, derived from the TRUSTED per-org namespace the control plane
	// placed the pool in (mitos-org-<id>), NEVER from a client-set field or label
	// on the input object. The org label is the key the usage pipeline meters by,
	// so a client must not be able to bill another org by setting it. We always
	// derive from pod.Namespace (== the pool/sandbox namespace): if it is an org
	// namespace, stamp mitos.run/org=<id>; if it is a non-org namespace (self-host
	// single-tenant), leave the pod unattributed (no org label) rather than forcing
	// a bogus org. Any org label the caller put on the input is ignored: this set
	// is the controller's own label map, and the value comes only from the
	// namespace.
	if orgID, ok := tenant.OrgFromNamespace(pod.Namespace); ok {
		pod.Labels[tenant.OrgLabelKey] = orgID
	}

	// Name-based egress needs the kernel to route the guest /30 out the pod
	// uplink, which requires net.ipv4.ip_forward=1 in the POD network namespace.
	// In Kubernetes the app container joins the pod netns (it does not create it),
	// so the runtime leaves /proc/sys/net read-only and the stub cannot write it.
	// A SHORT-LIVED privileged init container (privileged unmasks /proc/sys rw)
	// sets it in the shared netns and exits before the workload runs; the setting
	// persists for the pod's life. This needs NO node change (vs an unsafe kubelet
	// sysctl), and the privilege is bounded to a one-shot init container, not the
	// long-lived stub. It runs in the privileged-PSA namespace husk already
	// requires (NET_ADMIN + hostPath). Added only when name egress is configured
	// (opts.DNSUpstream set); clusters not using it get no init container.
	if opts.DNSUpstream != "" {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{
			Name:    "enable-ip-forward",
			Image:   opts.StubImage,
			Command: []string{"sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"},
			SecurityContext: &corev1.SecurityContext{
				Privileged: ptrBool(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
		})
	}

	// Owner-ref to the pool so Kubernetes garbage collection deletes husk pods
	// when the pool is deleted. c.Scheme() is the manager scheme (it carries
	// core/v1 and mitos.run/v1). An error here means the scheme is
	// missing a type and is a programming error; the caller logs and skips.
	// buildForkChildPod reuses this shape with a Client-less throwaway reconciler
	// and sets its OWN owner ref (to the SandboxFork) afterward, so skip the
	// pool owner ref when no Client (hence no scheme) is available.
	if r.Client != nil {
		_ = controllerutil.SetControllerReference(pool, pod, r.Scheme())
	}
	return pod
}

// buildForkChildPod builds a husk pod that activates from a fork snapshot. It
// reuses the same pod shape buildHuskPod emits (buildHuskPod needs a pool for
// GenerateName/namespace; a fork child is owned by the SandboxFork, not a pool),
// then rewrites the ownership, labels, and name for the fork. The pod is pinned
// to the source node (opts.ForkSourceNode) and activates from
// <DataDir>/forks/<ForkSnapshotID> (opts.ForkSnapshotID), both set by the caller.
// srcPod is that source node's SOURCE husk pod; the child inherits its
// scheduling constraints (nodeSelector and tolerations) so it can actually
// land next to it (see the inheritance comment in the body).
func buildForkChildPod(fork *v1.Sandbox, srcPod *corev1.Pod, childName string, opts HuskPodOptions, scheme *runtime.Scheme) *corev1.Pod {
	// Inherit the SOURCE pod's scheduling constraints so the child can actually
	// land on the source node. The child is pinned to that exact node by the
	// ForkSourceNode nodeAffinity (the fork snapshot and the source rootfs are
	// node-local hostPaths), but affinity does not clear taints: hosted KVM
	// nodes carry mitos.run/dedicated:NoSchedule, which warm pods tolerate via
	// the pool's spec.placement tolerations. The fork path has no pool in hand,
	// and the pool's placement may have changed since the source scheduled, so
	// the source pod's OWN spec is the authoritative record of what it took to
	// land there; without this the child sits Pending forever (production
	// FailedScheduling: "1 node(s) had untolerated taint {mitos.run/dedicated}")
	// and the fork 504s. The pin deliberately stays scheduler-visible
	// (nodeAffinity, NOT spec.nodeName): husk pods are scheduler-placed
	// throughout this file, and the KVM extended-resource request must pass
	// scheduler fit; spec.nodeName would bypass that and turn node capacity
	// exhaustion into a terminal kubelet OutOf<kvm-resource> pod instead of a
	// Pending pod that schedules when a slot frees.
	if srcPod != nil {
		opts.PlacementNodeSelector = srcPod.Spec.NodeSelector
		opts.PlacementTolerations = forkChildInheritedTolerations(srcPod.Spec.Tolerations)
	}

	// buildHuskPod only reads r.Scheme() (for the owner ref we overwrite) and the
	// opts, so a zero reconciler is sufficient to build the spec. A synthetic pool
	// carrier supplies GenerateName/namespace; ownership and labels are overwritten
	// below.
	r := &SandboxPoolReconciler{}
	carrier := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: fork.Name, Namespace: fork.Namespace}}
	// Build from the resolved SOURCE pool template so the fork child inherits the
	// SAME cpu burst cap + memory a warm-claimed sandbox of the pool gets, not the
	// default caps an empty template yields (issue #760). Nil falls back to the
	// empty template (default caps), which is the pre-fix behavior and safe.
	template := opts.Template
	if template == nil {
		template = &v1.PoolTemplateSpec{}
	}
	pod := r.buildHuskPod(carrier, template, opts)

	// Rewrite identity: owned by the SandboxFork (GC with it), labeled as a fork
	// child (never a warm-pool slot), deterministic name so re-reconcile is
	// idempotent.
	pod.OwnerReferences = nil
	pod.GenerateName = ""
	pod.Name = childName
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	delete(pod.Labels, huskPoolLabel)
	pod.Labels[huskLabel] = "true"
	pod.Labels[huskForkLabel] = fork.Name
	// The claim label carries the hosted sandbox id for a claimed pod; a fork
	// child is claimed by its fork Sandbox from birth. The usage scraper
	// (HuskPodScrapeLister) selects on this label, so a fork child without it
	// is silently unbilled, and the claim-label pod-deletion paths (release,
	// lifetime terminate) reap by it.
	pod.Labels[huskClaimLabel] = fork.Name
	_ = controllerutil.SetControllerReference(fork, pod, scheme)
	return pod
}

// reconcileHuskPods drives the warm pool toward pool.Spec.Replicas husk pod
// objects and returns the resulting count.
//
// Readiness nuance (envtest vs production): in production a husk slot is "ready"
// only when its pod is Running AND Ready (the dormant VMM is up and serving the
// control socket); the warm-pool size would gate on that. envtest has no
// kubelet, so pods never run, never go Ready, and have no phase. To keep the
// reconcile convergent under envtest AND in production we count by object
// EXISTENCE of non-terminating husk pods: create up to Replicas, delete the
// extras. A production readiness gate (Running+Ready before counting a slot
// warm) is layered on in the activation slice; object existence is the correct
// convergence target for this object-lifecycle slice.
// huskReconcileResult reports the post-reconcile warm-pool counts and whether
// this reconcile changed the dormant count (a scale event), so the caller can
// emit metrics and status without re-listing pods.
type huskReconcileResult struct {
	dormant  int32 // unclaimed warm pods after reconcile
	inUse    int32 // claimed/active pods (the demand signal)
	scaledUp bool  // dormant count increased this reconcile
	scaledDn bool  // dormant count decreased this reconcile

	// dormantPods is the dormant (unclaimed, non-stale-digest) husk pod set as
	// listed at the start of this reconcile, before any scale up/down churn. The
	// pool reconcile passes it to templateRestoreFailing (#584) so a crashloop
	// count is evaluated against live pod status without a second List call.
	dormantPods []corev1.Pod
}

// countHuskPods lists the pool's husk pods and returns the current dormant and
// in-use counts WITHOUT creating or deleting anything, so the autoscaler can
// compute a desired count before reconcileHuskPods drives toward it. Same
// filtering as reconcileHuskPods (non-terminating, owned, dormant vs claimed).
func (r *SandboxPoolReconciler) countHuskPods(ctx context.Context, pool *v1.SandboxPool) (dormant, inUse int32, err error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{huskPoolLabel: pool.Name, huskLabel: "true"},
	); err != nil {
		return 0, 0, fmt.Errorf("list husk pods for pool %s: %w", pool.Name, err)
	}
	var owned, dorm int32
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if owner := metav1.GetControllerOf(&p); owner == nil || owner.UID != pool.UID {
			continue
		}
		owned++
		if _, claimed := p.Labels[huskClaimLabel]; !claimed {
			dorm++
		}
	}
	use := owned - dorm
	if use < 0 {
		use = 0
	}
	return dorm, use, nil
}

func (r *SandboxPoolReconciler) reconcileHuskPods(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec, desired int32) (huskReconcileResult, error) {
	logger := log.FromContext(ctx)

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{huskPoolLabel: pool.Name, huskLabel: "true"},
	); err != nil {
		return huskReconcileResult{}, fmt.Errorf("list husk pods for pool %s: %w", pool.Name, err)
	}

	// Keep only non-terminating pods this pool actually owns. A pod with a
	// DeletionTimestamp is on its way out and must not count toward the warm
	// size (otherwise a scale-down would never converge).
	owned := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if owner := metav1.GetControllerOf(&p); owner == nil || owner.UID != pool.UID {
			continue
		}
		owned = append(owned, p)
	}

	// Issue #461: a warm husk pod bakes the snapshot digest it verifies against.
	// If the pool's snapshot is rebuilt under the same name (same templateID, new
	// mem + new digest), an existing dormant pod re-hashes the NEW mem against its
	// OLD manifest and CrashLoopBackOffs forever. Reap dormant pods whose stamped
	// digest no longer matches their node's current recorded digest so the scale-up
	// below refills them against the fresh snapshot. Claimed pods (a tenant VM) are
	// never reaped.
	templateID := poolTemplateID(pool)
	kept := owned[:0:0]
	for i := range owned {
		p := owned[i]
		if r.huskPodHasStaleDigest(&p, templateID) {
			if err := r.Delete(ctx, &p); err != nil && !apierrors.IsNotFound(err) {
				return huskReconcileResult{}, fmt.Errorf("delete stale husk pod %s/%s: %w", p.Namespace, p.Name, err)
			}
			logger.Info("reaped husk pod with stale snapshot digest", "pod", p.Name, "node", p.Annotations[huskSnapshotNodeAnnotation])
			continue
		}
		// Issue #679: the digest reap only covers pods that were stamped with a
		// digest. Pods created when NO digest was known (the fallback fleet) are
		// covered by the build generation instead: an in-place rebuild bumps the
		// pool's generation, and any dormant pod stamped with an older one (or
		// not stamped at all, the pre-#679 legacy fleet) holds a rootfs clone of
		// the OLD artifacts and must be replaced.
		if huskPodStaleByGeneration(&p, pool.Status.TemplateBuildGeneration) {
			if err := r.Delete(ctx, &p); err != nil && !apierrors.IsNotFound(err) {
				return huskReconcileResult{}, fmt.Errorf("delete stale-generation husk pod %s/%s: %w", p.Namespace, p.Name, err)
			}
			logger.Info("reaped husk pod with stale build generation", "pod", p.Name, "podGeneration", p.Annotations[huskBuildGenerationAnnotation], "poolGeneration", pool.Status.TemplateBuildGeneration)
			continue
		}
		kept = append(kept, p)
	}
	owned = kept

	// Record refill latency (create -> first Ready dormant) for any warm pod not
	// yet observed, then mark it so it is counted exactly once.
	r.observeRefillForReadyPods(ctx, owned)

	// Count only UNCLAIMED (dormant) pods toward the warm target. A pod carrying
	// the claim label has been consumed by a SandboxClaim: it is activating or
	// active, holding tenant state, and is NOT a warm slot. Counting it would
	// leave the pool one warm pod short for every outstanding claim (the slot a
	// claim took is never refilled), which is exactly the "no warm husk pod is
	// ready" stall. Excluding claimed pods makes the pool maintain Replicas
	// DORMANT pods, refilling each slot a claim consumes; the total pod count is
	// then Replicas (warm) + the number of active claims.
	dormant := make([]corev1.Pod, 0, len(owned))
	for i := range owned {
		if _, claimed := owned[i].Labels[huskClaimLabel]; claimed {
			continue
		}
		dormant = append(dormant, owned[i])
	}

	existing := int32(len(dormant))
	inUse := int32(len(owned)) - existing
	if inUse < 0 {
		inUse = 0
	}
	result := huskReconcileResult{dormant: existing, inUse: inUse, dormantPods: dormant}

	switch {
	case existing < desired:
		deficit := desired - existing
		logger.Info("husk pod deficit", "dormant", existing, "desired", desired, "creating", deficit)
		// Replicate the husk PKI secrets into this pool's namespace before
		// creating pods: husk pods run here, not in the controller namespace,
		// and mount mitos-ca + mitos-forkd-tls. ControllerNamespace empty (or
		// equal to the pool namespace) makes this a noop. A replication error
		// is returned so the deficit is retried on requeue rather than creating
		// pods that would fail to mount their secrets.
		if r.ControllerNamespace != "" {
			if err := ReplicateHuskSecrets(ctx, r.Client, r.ControllerNamespace, pool.Namespace); err != nil {
				result.dormant = existing
				return result, fmt.Errorf("replicate husk secrets into %s: %w", pool.Namespace, err)
			}
			// Issue this namespace's own husk control-channel server leaf
			// (husk.<ns>.mitos) so the husk pod serves the control channel with a
			// per-namespace identity and the shared forkd server key is never
			// replicated here. Fail-closed: a husk pod is not created without it.
			if err := EnsureHuskTLS(ctx, r.Client, r.ControllerNamespace, pool.Namespace); err != nil {
				result.dormant = existing
				return result, fmt.Errorf("ensure husk tls in %s: %w", pool.Namespace, err)
			}
			// Grant the controller namespaced Secrets access here via a RoleBinding
			// to the mitos-pool-secrets ClusterRole, so the cluster-wide Secrets
			// grant can be removed (the controller reaches Secrets only in adopted
			// pool namespaces). This is ADDITIVE groundwork: it is load-bearing only
			// once namespacedSecretsRBAC is enabled (which by construction also
			// grants the controller the `bind`/`rolebindings` permission). While the
			// cluster-wide grant is still present (the default, and any deploy that
			// has not surfaced the new RBAC) the controller works without it, so a
			// failure here is NON-FATAL: log and keep creating husk pods rather than
			// breaking the warm pool on a rollout-ordering or RBAC-mirror gap.
			if err := EnsurePoolSecretsRoleBinding(ctx, r.Client, r.ControllerNamespace, pool.Namespace); err != nil {
				logger.V(1).Info("pool secrets RoleBinding not ensured; continuing (needed only when namespacedSecretsRBAC is enabled)", "namespace", pool.Namespace, "err", err.Error())
			}
		}
		opts := HuskPodOptions{
			MultiVM:            r.MultiVM,
			LiveCowFork:        r.LiveCowFork,
			LiveCowChildImport: r.LiveCowChildImport,
			PrewarmChild:       r.PrewarmChild,
			PrepareEgressLink:  r.PrepareEgressLink,
			MultiVMForkVMs:     r.MultiVMForkVMs,
			StubImage:          r.HuskStubImage,
			DNSUpstream:        r.HuskDNSUpstream,
			KVMResourceName:    r.KVMResourceName,
			SnapshotID:         poolTemplateID(pool),
			DataDir:            r.DataDir,
			TLSSecretName:      r.HuskTLSSecretName,
			CASecretName:       r.HuskCASecretName,
			// dedicatedNodes (#172): pin this pool's husk pods to the tenant's
			// dedicated node set. Merged onto the KVM nodeSelector + snapshot-node
			// affinity below.
			PlacementNodeSelector: huskPlacementNodeSelector(pool),
			PlacementTolerations:  huskPlacementTolerations(pool),
			// ExpectedDigest and SnapshotNodes are assigned PER POD below. Each node
			// builds its template snapshot independently, so the recorded
			// content-addressed digests differ per node. A husk pod must be pinned to
			// exactly ONE snapshot node and verify against THAT node's digest;
			// handing every pod a single cluster-wide digest makes pods that land on
			// any other node fail prepare-time snapshot verification (issue #175).
		}
		// Snapshot holders with their own per-node digests, balanced across so a
		// pool spreads its warm pods (and so a lost node's slots refill onto the
		// survivors, where the digest is correct for that node).
		var holders []SnapshotHolder
		if r.NodeRegistry != nil {
			holders = r.NodeRegistry.SnapshotHolders(poolTemplateID(pool))
		}
		// dedicatedNodes (#172): a placed pool may only run on its dedicated nodes,
		// so restrict the snapshot-holders we pin pods to (the #175 per-node-digest
		// assignment) to holders that ALSO match the placement nodeSelector.
		// Otherwise a pod assigned to a holder outside the placement set gets a
		// node affinity that conflicts with the placement nodeSelector and stays
		// Pending forever.
		if sel := huskPlacementNodeSelector(pool); len(sel) > 0 && len(holders) > 0 {
			matching, err := r.nodesMatchingSelector(ctx, sel)
			if err != nil {
				result.dormant = existing
				return result, fmt.Errorf("list placement nodes for pool %s: %w", pool.Name, err)
			}
			filtered := holders[:0:0]
			for _, h := range holders {
				if matching[h.Name] {
					filtered = append(filtered, h)
				}
			}
			holders = filtered
		}
		assigned := map[string]int{}
		for i := range owned {
			if n := owned[i].Spec.NodeName; n != "" {
				assigned[n]++
			}
		}
		for i := int32(0); i < deficit; i++ {
			podOpts := opts
			if len(holders) > 0 {
				// Pick the least-loaded holder so the deficit spreads evenly.
				pick := holders[0]
				for _, h := range holders {
					if assigned[h.Name] < assigned[pick.Name] {
						pick = h
					}
				}
				assigned[pick.Name]++
				podOpts.SnapshotNodes = []string{pick.Name}
				podOpts.ExpectedDigest = pick.Digest
			} else {
				// No registry, or no node has reported holding the snapshot yet: fall
				// back to the kvm nodeSelector alone and the stub's unverified escape
				// hatch so a pre-digest pool still warms.
				podOpts.SnapshotNodes = r.snapshotNodeNames(poolTemplateID(pool))
				podOpts.ExpectedDigest = r.huskTemplateDigest(poolTemplateID(pool))
			}
			pod := r.buildHuskPod(pool, template, podOpts)
			if err := r.Create(ctx, pod); err != nil {
				result.dormant = existing
				return result, fmt.Errorf("create husk pod for pool %s: %w", pool.Name, err)
			}
			recordHuskPodCreated(poolKey(pool))
			existing++
		}
		result.scaledUp = true

	case existing > desired:
		// Delete the extras deterministically from the DORMANT set only (never a
		// claimed/active pod, which holds a tenant's running VM): sort by name and
		// delete the tail (newest GenerateName suffixes sort last), so repeated
		// reconciles pick the same victims and the set converges.
		sort.Slice(dormant, func(i, j int) bool { return dormant[i].Name < dormant[j].Name })
		surplus := existing - desired
		logger.Info("husk pod surplus", "dormant", existing, "desired", desired, "deleting", surplus)
		for i := int32(0); i < surplus; i++ {
			victim := dormant[len(dormant)-1-int(i)]
			if err := r.Delete(ctx, &victim); err != nil && !apierrors.IsNotFound(err) {
				result.dormant = existing
				return result, fmt.Errorf("delete surplus husk pod %s: %w", victim.Name, err)
			}
			existing--
		}
		result.scaledDn = true
	}

	result.dormant = existing
	return result, nil
}

func ptrBool(b bool) *bool { return &b }

func ptrInt64(i int64) *int64 { return &i }

// mergedHuskNodeSelector is the KVM node label plus the pool's spec.placement
// nodeSelector (dedicatedNodes, #172), so a husk pod (and its VM) runs only on
// the tenant's dedicated KVM nodes. Placement keys win on a collision.
func mergedHuskNodeSelector(opts HuskPodOptions) map[string]string {
	sel := map[string]string{huskKVMNodeLabel: "true"}
	for k, v := range opts.PlacementNodeSelector {
		sel[k] = v
	}
	return sel
}

// huskTolerations is the fast node-loss eviction tolerations (#177) plus the
// pool's spec.placement tolerations (#172) so a husk pod schedules onto tainted
// dedicated nodes.
func huskTolerations(opts HuskPodOptions) []corev1.Toleration {
	tol := []corev1.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptrInt64(huskNodeLossTolerationSeconds)},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptrInt64(huskNodeLossTolerationSeconds)},
	}
	return append(tol, opts.PlacementTolerations...)
}

// forkChildInheritedTolerations returns the SOURCE husk pod's tolerations
// minus the fast node-loss pair (node.kubernetes.io/not-ready and
// node.kubernetes.io/unreachable) that huskTolerations re-adds to every husk
// pod, so a fork child carries each toleration exactly once. Everything else
// is copied verbatim: the pool placement tolerations the source scheduled with
// (#172, e.g. the mitos.run/dedicated NoSchedule taint on hosted KVM nodes)
// and anything admission injected for the source apply equally to the child,
// which must land on the SAME node.
func forkChildInheritedTolerations(src []corev1.Toleration) []corev1.Toleration {
	var out []corev1.Toleration
	for _, t := range src {
		if t.Key == corev1.TaintNodeNotReady || t.Key == corev1.TaintNodeUnreachable {
			continue
		}
		out = append(out, t)
	}
	return out
}

// huskPlacementNodeSelector / huskPlacementTolerations read the pool's
// spec.placement (dedicatedNodes, #172), nil-safe.
func huskPlacementNodeSelector(pool *v1.SandboxPool) map[string]string {
	if pool.Spec.Placement == nil {
		return nil
	}
	return pool.Spec.Placement.NodeSelector
}

func huskPlacementTolerations(pool *v1.SandboxPool) []corev1.Toleration {
	if pool.Spec.Placement == nil {
		return nil
	}
	return pool.Spec.Placement.Tolerations
}

// nodesMatchingSelector returns the set of node names whose labels match sel
// (a pool's placement nodeSelector), so husk-pod placement can be restricted to a
// tenant's dedicated nodes (#172). The cached client lazily starts a Node
// informer (cluster-wide get;list;watch RBAC, granted in the controller role).
func (r *SandboxPoolReconciler) nodesMatchingSelector(ctx context.Context, sel map[string]string) (map[string]bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels(sel)); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(nodes.Items))
	for i := range nodes.Items {
		out[nodes.Items[i].Name] = true
	}
	return out, nil
}

// huskNodeLossTolerationSeconds is how long a husk pod tolerates its node being
// NotReady/unreachable before Kubernetes evicts it (vs the 300s default). Chosen
// longer than a typical node reboot (~40-60s, ridden out without eviction) but
// far below 5min so a real node loss fails an active claim over and refills the
// warm pool in ~a minute (#177).
const huskNodeLossTolerationSeconds int64 = 60

// huskPodHasStaleDigest reports whether a husk pod's stamped snapshot digest no
// longer matches its pinned node's current recorded digest for the template
// (issue #461). A claimed (activating/active) pod is never stale here: it holds a
// tenant VM and must not be reaped. A pod with no stamped digest/node (the
// pre-digest fallback or a fork child) or a node with no currently known digest
// is treated as NOT stale, so steady state never churns.
func (r *SandboxPoolReconciler) huskPodHasStaleDigest(p *corev1.Pod, templateID string) bool {
	if p.Labels[huskClaimLabel] != "" {
		return false
	}
	stamped := p.Annotations[huskTemplateDigestAnnotation]
	// The node to compare against: the stamped pin when the pod was pinned to a
	// single snapshot node, otherwise the node the scheduler actually placed the
	// pod on (the multi-node and fallback paths, issue #679). An unscheduled pod
	// is undecidable and never reaped here.
	node := p.Annotations[huskSnapshotNodeAnnotation]
	if node == "" {
		node = p.Spec.NodeName
	}
	if stamped == "" || node == "" || r.NodeRegistry == nil {
		return false
	}
	current, known := r.NodeRegistry.TemplateDigestOnNode(node, templateID)
	if !known {
		return false
	}
	return current != stamped
}

// huskPodStaleByGeneration reports whether a DORMANT husk pod was created under
// an older template build generation than the pool's current one (issue #679).
// It is the rebuild-reap signal that works with no digest at all: after an
// in-place rebuild bumps the pool's generation, a pod stamped with an older
// generation (or not stamped at all, the pre-#679 fallback fleet) holds a
// rootfs CoW clone of the OLD artifacts; activating the new snapshot against it
// is exactly the mem-vs-rootfs skew the reap exists to prevent. A claimed pod
// is never stale (it holds a tenant VM). Before any rebuild (generation 0) an
// unstamped pod is fine: its clone is from the only build there has ever been.
func huskPodStaleByGeneration(p *corev1.Pod, poolGeneration int64) bool {
	if p.Labels[huskClaimLabel] != "" {
		return false
	}
	if poolGeneration == 0 {
		return false
	}
	return p.Annotations[huskBuildGenerationAnnotation] != strconv.FormatInt(poolGeneration, 10)
}

// refillObservedAnnotation marks a husk pod whose create-to-Ready refill latency
// has already been recorded, so the histogram counts each pod exactly once.
const refillObservedAnnotation = "mitos.run/refill-observed"

// observeRefillForReadyPods records, once per pod, the wall-clock from a husk
// pod's creation to it first being seen Ready and dormant (a warm slot), the
// refill cost an operator watches after a scale-up. The pod is patched with the
// observed marker BEFORE the histogram is observed, so a patch failure means the
// metric is recorded late on a later reconcile rather than double-counted. Best
// effort: a patch failure is logged at low verbosity and retried next reconcile.
func (r *SandboxPoolReconciler) observeRefillForReadyPods(ctx context.Context, owned []corev1.Pod) {
	logger := log.FromContext(ctx)
	for i := range owned {
		p := owned[i]
		if p.Labels[huskClaimLabel] != "" || !huskPodReady(&p) {
			continue
		}
		if p.Annotations[refillObservedAnnotation] != "" {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[refillObservedAnnotation] = "true"
		if err := r.Patch(ctx, &p, patch); err != nil {
			logger.V(1).Info("mark husk pod refill-observed failed; will retry", "pod", p.Name, "err", err.Error())
			continue
		}
		observeRefillLatency(r.now().Sub(p.CreationTimestamp.Time).Seconds())
	}
}

// huskPodOwnedByPool reports whether pod carries the controller owner reference
// reconcileHuskPods stamps for this pool: a controller reference (Controller=true)
// of Kind SandboxPool naming this pool with BlockOwnerDeletion set. It is the
// unforgeable provenance signal the warm slot selector requires before treating
// a pod as an activation target, so a tenant who can only create pods (and set
// arbitrary labels) in the pool namespace cannot plant a decoy the controller
// would hand secrets to.
//
// The forgery barrier is BlockOwnerDeletion=true: the
// OwnerReferencesPermissionEnforcement admission plugin refuses to let a
// principal set it on an owner whose finalizers subresource they cannot update,
// so a tenant cannot reference THIS named pool with that bit. The owner UID is
// deliberately NOT compared: it adds no forgery resistance (anyone who could
// forge BlockOwnerDeletion could also read and copy the pool UID), and in
// production GC already deletes husk pods whose owner UID drifts from the pool.
func huskPodOwnedByPool(pod *corev1.Pod, pool *v1.SandboxPool) bool {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false
	}
	return ref.Kind == "SandboxPool" &&
		ref.Name == pool.Name &&
		ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion
}

// huskPodReady reports whether a husk pod is a usable dormant slot: Running,
// with a Ready condition True, and a non-empty PodIP (so the controller can
// dial its control channel and set a reachable endpoint).
// huskPodNotReadyReason renders, in one short phrase, why a husk pod is not usable
// yet. The fan-out and claim paths skip such a pod and retry; without a reason a
// permanently stuck pod is indistinguishable from one that is still coming up.
// Returns "" for a pod that IS ready.
func huskPodNotReadyReason(p *corev1.Pod) string {
	if p.Status.Phase != corev1.PodRunning {
		return fmt.Sprintf("pod phase %s", p.Status.Phase)
	}
	if p.Status.PodIP == "" {
		return "pod has no IP yet"
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			if c.Status == corev1.ConditionTrue {
				return ""
			}
			if c.Reason != "" {
				return "pod not ready: " + c.Reason
			}
			return "pod not ready"
		}
	}
	return "pod has no Ready condition"
}

func huskPodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// selectDormantHuskPod returns one Running+Ready husk pod for the pool that has
// a PodIP and is not yet claimed (no mitos.run/claim label). It is the warm
// slot the claim path activates. Returns nil (no error) when none is available,
// so the caller pends the claim. Selection is deterministic (lowest name) so
// concurrent reconciles converge on the same victim; the optimistic-lock
// claim-label patch in markHuskPodClaimed is the real commit: two concurrent
// claims that both select the SAME pod both attempt the patch, but the patch
// carries the pod's resourceVersion so exactly one wins and the loser gets a 409
// Conflict and requeues to pick a different dormant pod. A pod is therefore
// claimed (and activated) by exactly one claim.
func (r *SandboxReconciler) selectDormantHuskPod(ctx context.Context, pool *v1.SandboxPool) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{huskPoolLabel: pool.Name, huskLabel: "true"},
	); err != nil {
		return nil, fmt.Errorf("list husk pods for pool %s: %w", pool.Name, err)
	}

	var candidates []corev1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Labels[huskClaimLabel] != "" {
			continue
		}
		if !huskPodReady(&p) {
			continue
		}
		// Provenance gate: activation delivers the claim's resolved secrets and
		// per-sandbox bearer token to the pod's self-reported IP, so the pod must
		// be one the controller actually created, not merely something carrying
		// the husk labels (which any tenant with pod-create in this namespace can
		// set). reconcileHuskPods stamps a controller owner reference to the pool;
		// requiring it here means a tenant-planted decoy is never an activation
		// target. The BlockOwnerDeletion bit it carries is protected by the
		// OwnerReferencesPermissionEnforcement admission plugin: a tenant cannot
		// forge it without update on this pool's finalizers subresource.
		if !huskPodOwnedByPool(&p, pool) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	chosen := candidates[0]
	return &chosen, nil
}

// markHuskPodClaimed stamps the mitos.run/claim label on a husk pod so it is
// not selected again. It uses an OPTIMISTIC-LOCK merge patch: the patch carries
// the pod's resourceVersion, so the API server rejects it with a 409 Conflict if
// the pod was modified (for instance, claimed by a racing reconcile) since this
// reconcile read it. This is the mutual-exclusion guarantee: two concurrent
// claims that both selected the same dormant pod both attempt this patch, but
// only one wins; the other gets apierrors.IsConflict and must NOT activate this
// pod (the caller requeues to pick a different dormant pod). The label-only
// patch still merges cleanly with concurrent kubelet status writes (status is a
// separate subresource), so the optimistic lock fires only on a genuine
// metadata race, which is exactly the double-assignment it must prevent.
func (r *SandboxReconciler) markHuskPodClaimed(ctx context.Context, pod *corev1.Pod, claim *v1.Sandbox) error {
	patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[huskClaimLabel] = claim.Name
	stampClaimOrgLabel(pod, claim)
	stampClaimRegionLabel(pod, claim)
	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("mark husk pod %s claimed by %s: %w", pod.Name, claim.Name, err)
	}
	return nil
}

// stampClaimOrgLabel fills the metering attribution org on a husk pod at CLAIM
// time (issue #602). The husk pod builder stamps mitos.run/org from the
// TRUSTED per-org namespace (mitos-org-<id>), which covers org tenancy; but a
// hosted SINGLE-TENANT deployment runs the pool and its husk pods in one shared
// namespace, so those pods carry no org label and every usage sample the
// collector scrapes is unattributed (dropped from billable). The hosted gateway
// stamps the org label on every Sandbox it creates (internal/saas/controlplane),
// so at the moment a claim wins a pod we copy that label over: fill when the
// pod has none, override when it differs (a stale label from another org's
// failed activation).
//
// Trust boundary: in a per-org namespace the NAMESPACE stays authoritative and
// the claim label never overrides it; a tenant with direct access to its org
// namespace could otherwise label its Sandbox with another org and bill them.
// In a shared namespace only the control plane (the hosted gateway or the
// operator) creates Sandboxes, so the claim's label is control-plane identity,
// not tenant input. A claim without an org label stamps nothing: the pod stays
// unattributed rather than being forced into a default org.
func stampClaimOrgLabel(pod *corev1.Pod, claim *v1.Sandbox) {
	if _, ok := tenant.OrgFromNamespace(pod.Namespace); ok {
		return
	}
	org := claim.Labels[tenant.OrgLabelKey]
	if org == "" {
		return
	}
	pod.Labels[tenant.OrgLabelKey] = org
}

// stampClaimRegionLabel copies the claim's placement region (issue #712 phase
// 0) onto the husk pod it wins at claim time, best-effort. A warm husk pod has
// no region of its own before it is claimed (it is pre-created from a pool,
// not a tree root); the claiming Sandbox's mitos.run/region label, when
// present, is the tree root's true region, so this is the one point that fact
// becomes visible on the pod the usage collector actually scrapes. Unlike the
// org label, this is NOT gated by tenancy mode: a per-org namespace carries no
// region information of its own, so the claim is always the source. A claim
// with no region label (a single-value deployment, or a sandbox predating
// this field) stamps nothing, leaving the pod's region empty rather than
// forcing a default.
func stampClaimRegionLabel(pod *corev1.Pod, claim *v1.Sandbox) {
	region := claim.Labels[tenant.RegionLabelKey]
	if region == "" {
		return
	}
	pod.Labels[tenant.RegionLabelKey] = region
}

// unmarkHuskPodClaimed removes the mitos.run/claim label so a husk pod returns
// to the dormant pool after a FAILED activation. The claim path stamps the label
// BEFORE activating (the mutual-exclusion commit); without releasing it on
// failure, a pod that was claimed but never finished activating (a transient
// transport error, or a per-node-digest mismatch when failing over to another
// node) keeps the label forever and is excluded from selectDormantHuskPod
// permanently, leaking warm capacity and blocking cross-node failover (#177). It
// is the claim that stamped the label releasing it, so no optimistic lock is
// needed; a no-op when the label is already absent.
func (r *SandboxReconciler) unmarkHuskPodClaimed(ctx context.Context, pod *corev1.Pod) error {
	if pod.Labels[huskClaimLabel] == "" {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Labels, huskClaimLabel)
	// Also release a CLAIM-STAMPED attribution org (issue #602): in a shared
	// (non-org) namespace the org label came from the failed claim, and a pod
	// returned to the dormant pool must not carry it into a later claim that has
	// none (a misattribution). In a per-org namespace the label is derived from
	// the trusted namespace at pod creation and stays valid, so it is kept.
	if _, ok := tenant.OrgFromNamespace(pod.Namespace); !ok {
		delete(pod.Labels, tenant.OrgLabelKey)
	}
	// The region label (issue #712 phase 0) is ALWAYS claim-stamped, never
	// namespace-derived (see stampClaimRegionLabel), so a failed claim's
	// region must always be released here, unlike the org label above.
	delete(pod.Labels, tenant.RegionLabelKey)
	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("release husk pod %s claim label: %w", pod.Name, err)
	}
	return nil
}
