package controller_test

// Coverage for the husk pod warm-pool lifecycle (issue #18, slice 1).
//
// Two layers:
//   - a pure unit test of buildHuskPod that asserts the spec the controller
//     emits: the mitos.run/kvm request+limit, the documented non-privileged
//     securityContext, the owner-ref to the pool, the two husk labels, the
//     cpu/memory requests, and the stub image.
//   - an envtest of reconcileHuskPods that drives the warm pool through create
//     (Replicas=3 -> 3 husk pod objects owned by the pool), scale-down
//     (Replicas=1 -> 2 deleted), and the flag-off case (no husk pods). envtest
//     has no kubelet, so the pods never run; the reconcile converges on object
//     EXISTENCE, which this test asserts (the real-vs-envtest readiness nuance
//     is documented in huskpod.go).

import (
	v1 "mitos.run/mitos/api/v1"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBuildHuskPodSpec(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "spec-pool", Namespace: "default", UID: "pool-uid-1"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image: "python:3.12-slim",
				Resources: v1.SandboxResources{
					CPU:    resource.MustParse("2"),
					Memory: resource.MustParse("1Gi"),
				},
			},
			Warm: &v1.PoolWarm{Min: 2},
		},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:       "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	})

	if pod.GenerateName != "spec-pool-husk-" {
		t.Errorf("GenerateName = %q, want spec-pool-husk-", pod.GenerateName)
	}
	if pod.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", pod.Namespace)
	}
	if pod.Labels["mitos.run/pool"] != "spec-pool" {
		t.Errorf("pool label = %q, want spec-pool", pod.Labels["mitos.run/pool"])
	}
	if pod.Labels["mitos.run/husk"] != "true" {
		t.Errorf("husk label = %q, want true", pod.Labels["mitos.run/husk"])
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want Always", pod.Spec.RestartPolicy)
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "spec-pool" {
		t.Fatalf("controller owner = %+v, want SandboxPool spec-pool", owner)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	ctr := pod.Spec.Containers[0]
	if ctr.Name != "husk-stub" {
		t.Errorf("container name = %q, want husk-stub", ctr.Name)
	}
	if ctr.Image != "mitos-husk-stub:test" {
		t.Errorf("container image = %q, want mitos-husk-stub:test", ctr.Image)
	}

	kvm := corev1.ResourceName("mitos.run/kvm")
	if got := ctr.Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm request = %s, want 1", got.String())
	}
	if got := ctr.Resources.Limits[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm limit = %s, want 1", got.String())
	}
	// CPU: limit = configured cap (burst ceiling enforced by cgroup cpu.max);
	// request = low floor for node overcommit (scheduler packs by request, so idle
	// warm husks pack densely; each VM bursts up to the limit when active).
	// Memory: request = configured, limit = request + headroom (no overcommit; guest
	// RAM is genuinely resident in Firecracker and overcommitting it would OOM-kill
	// live sandboxes under node pressure).
	if got := ctr.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("cpu request = %s, want 50m (overcommit floor)", got.String())
	}
	if got := ctr.Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("cpu limit = %s, want 2 (configured cap from template)", got.String())
	}
	if got := ctr.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory request = %s, want 1Gi (from template)", got.String())
	}
	// Memory LIMIT = request + headroom (production-blocker #2, cap 1): the VM
	// at its configured RAM is never OOM-killed but a runaway is capped. Default
	// headroom max(256Mi, 25%); for 1Gi that is 256Mi.
	wantMemLimit := resource.MustParse("1Gi")
	wantMemLimit.Add(resource.MustParse("256Mi"))
	if got := ctr.Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantMemLimit) != 0 {
		t.Errorf("memory limit = %s, want %s (request + headroom)", got.String(), wantMemLimit.String())
	}

	sc := ctr.SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("Privileged must be explicitly false")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be explicitly false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop = %+v, want [ALL]", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_ADMIN" {
		t.Errorf("Capabilities.Add = %+v, want [NET_ADMIN] (in-pod egress firewall)", sc.Capabilities.Add)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
	}
}

// TestBuildHuskPodCPULimitOvercommit verifies the overcommit model: when
// spec.template.resources.cpu is set the husk container has Limits[cpu] equal to
// that value (the burst cap, enforced by cgroup cpu.max) and Requests[cpu] equal
// to the low floor (50m), so idle warm husks pack densely while each VM can burst
// to its declared cap. When cpu is unset the defaults apply: Limits[cpu] = 250m
// and Requests[cpu] = 50m (floor < default, so floor wins). Memory is left
// honest (request = configured, limit = request + headroom) with no overcommit.
func TestBuildHuskPodCPULimitOvercommit(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}

	// Case 1: explicit cpu in template (2 cores).
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Resources: v1.SandboxResources{
					CPU:    resource.MustParse("2"),
					Memory: resource.MustParse("1Gi"),
				},
			},
		},
	}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "img"})
	ctr := pod.Spec.Containers[0]

	if got := ctr.Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("case explicit cpu=2: Limits[cpu] = %s, want 2", got.String())
	}
	if got := ctr.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("case explicit cpu=2: Requests[cpu] = %s, want 50m (floor)", got.String())
	}
	// Memory must stay honest: request equals configured, limit equals request + headroom.
	if got := ctr.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("case explicit cpu=2: Requests[memory] = %s, want 1Gi", got.String())
	}

	// Case 2: no cpu in template; defaults kick in.
	poolDefault := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "ns"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "img"},
		},
	}
	podDefault := r.BuildHuskPodForTest(poolDefault, poolDefault.Spec.Template, controller.HuskPodOptions{StubImage: "img"})
	ctrDefault := podDefault.Spec.Containers[0]

	// defaultHuskCPU = 250m; floor = 50m; 50m < 250m so Requests[cpu] = 50m.
	if got := ctrDefault.Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("250m")) != 0 {
		t.Errorf("case default cpu: Limits[cpu] = %s, want 250m (defaultHuskCPU)", got.String())
	}
	if got := ctrDefault.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("case default cpu: Requests[cpu] = %s, want 50m (floor)", got.String())
	}
}

// TestBuildHuskPodThreadsDNSUpstream proves the controller passes a configured
// DNS upstream list to the husk-stub as --dns-upstream (the wiring that turns on
// name-based egress), and omits the flag entirely when no upstream is configured
// so the stub stays in IP-only allowlist mode.
func TestBuildHuskPodThreadsDNSUpstream(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	withDNS := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{
		StubImage:   "img",
		DNSUpstream: "1.1.1.1:53,8.8.8.8:53",
	})
	if !argsContainPair(withDNS.Spec.Containers[0].Args, "--dns-upstream", "1.1.1.1:53,8.8.8.8:53") {
		t.Errorf("husk args missing --dns-upstream pair: %v", withDNS.Spec.Containers[0].Args)
	}
	// Name egress also needs ip_forward in the pod netns, set by a short-lived
	// privileged init container (the workload container cannot write the read-only
	// /proc/sys/net). No node/kubelet change required.
	if !hasForwardInit(withDNS.Spec.InitContainers) {
		t.Errorf("husk pod missing the ip_forward init container when name egress on: %+v", withDNS.Spec.InitContainers)
	}

	withoutDNS := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img"})
	for _, a := range withoutDNS.Spec.Containers[0].Args {
		if a == "--dns-upstream" {
			t.Errorf("husk args must omit --dns-upstream when unset: %v", withoutDNS.Spec.Containers[0].Args)
		}
	}
	// No name egress => no init container, so clusters not using it are unaffected.
	if hasForwardInit(withoutDNS.Spec.InitContainers) {
		t.Errorf("husk pod must not add the ip_forward init container when name egress off: %+v", withoutDNS.Spec.InitContainers)
	}
}

// TestBuildHuskPodThreadsLiveCowFork proves the milestone m4b flag flows into the
// warm husk pod spec: with HuskPodOptions.LiveCowFork the stub gets --live-cow-fork
// so a co-located fork child can share the parent's resident guest memory, and
// with it OFF (the default, every deployment today) the flag is absent so the pod
// is byte-for-byte the current disk co-location.
func TestBuildHuskPodThreadsPrewarmChild(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	on := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", MultiVM: true, PrewarmChild: true})
	if !argsContain(on.Spec.Containers[0].Args, "--prewarm-child") {
		t.Errorf("husk args missing --prewarm-child when enabled: %v", on.Spec.Containers[0].Args)
	}

	off := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", MultiVM: true})
	if argsContain(off.Spec.Containers[0].Args, "--prewarm-child") {
		t.Errorf("husk args must omit --prewarm-child by default: %v", off.Spec.Containers[0].Args)
	}
}

func TestBuildHuskPodThreadsLiveCowChildImport(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	on := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", LiveCowFork: true, LiveCowChildImport: true})
	if !argsContain(on.Spec.Containers[0].Args, "--live-cow-child-import") {
		t.Errorf("husk args missing --live-cow-child-import when enabled: %v", on.Spec.Containers[0].Args)
	}

	off := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", LiveCowFork: true})
	if argsContain(off.Spec.Containers[0].Args, "--live-cow-child-import") {
		t.Errorf("husk args must omit --live-cow-child-import by default: %v", off.Spec.Containers[0].Args)
	}
}

func TestBuildHuskPodThreadsLiveCowFork(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	on := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", LiveCowFork: true})
	if !argsContain(on.Spec.Containers[0].Args, "--live-cow-fork") {
		t.Errorf("husk args missing --live-cow-fork when enabled: %v", on.Spec.Containers[0].Args)
	}
	// The live-cow write-protect fork creates its userfaultfd via the /dev/userfaultfd
	// device injected by the kvm device plugin (issue #832), NOT the userfaultfd(2)
	// syscall the container seccomp profile denies. So a live-cow husk pod keeps the
	// minimal NET_ADMIN-only capability set and adds NO CAP_SYS_PTRACE: the earlier
	// CAP_SYS_PTRACE grant satisfied only the kernel gate, never the seccomp gate, and
	// is not needed by the device path.
	if onCaps := on.Spec.Containers[0].SecurityContext.Capabilities.Add; capsContain(onCaps, "SYS_PTRACE") {
		t.Errorf("live-cow husk must NOT add SYS_PTRACE (device path needs no cap); Capabilities.Add = %+v", onCaps)
	}
	if onCaps := on.Spec.Containers[0].SecurityContext.Capabilities.Add; len(onCaps) != 1 || !capsContain(onCaps, "NET_ADMIN") {
		t.Errorf("live-cow husk Capabilities.Add = %+v, want exactly [NET_ADMIN]", onCaps)
	}

	off := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img"})
	if argsContain(off.Spec.Containers[0].Args, "--live-cow-fork") {
		t.Errorf("husk args must omit --live-cow-fork by default: %v", off.Spec.Containers[0].Args)
	}
	// A non-live-cow pod also stays NET_ADMIN-only.
	if offCaps := off.Spec.Containers[0].SecurityContext.Capabilities.Add; capsContain(offCaps, "SYS_PTRACE") {
		t.Errorf("non-live-cow husk must not add SYS_PTRACE; Capabilities.Add = %+v", offCaps)
	}
}

// capsContain reports whether a capability is present in the add list.
func capsContain(caps []corev1.Capability, want corev1.Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// argsContain reports whether a standalone flag is present in args.
func argsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// hasForwardInit reports whether a privileged init container enabling IPv4
// forwarding is present.
func hasForwardInit(inits []corev1.Container) bool {
	for _, c := range inits {
		if c.Name == "enable-ip-forward" && c.SecurityContext != nil &&
			c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			return true
		}
	}
	return false
}

func argsContainPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

// TestHuskPodHasNetAdminForInPodFirewall asserts the husk container adds
// exactly NET_ADMIN (and nothing else) so the stub can program the in-pod
// nftables egress filter and tap in the pod's OWN netns. This is the documented
// capability exception for in-pod firewalling; it does NOT grant host network
// access (the pod is not hostNetwork and not privileged).
func TestHuskPodHasNetAdminForInPodFirewall(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}
	pod := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img"})
	sc := pod.Spec.Containers[0].SecurityContext
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("Capabilities.Drop = %+v, want [ALL]", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_ADMIN" {
		t.Errorf("Capabilities.Add = %+v, want [NET_ADMIN]", sc.Capabilities.Add)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Error("husk pod must not be privileged")
	}
	if pod.Spec.HostNetwork {
		t.Error("husk pod must not use hostNetwork; NET_ADMIN is scoped to the pod netns")
	}
}

// TestBuildHuskPodPSARestricted asserts the husk pod satisfies every PSA
// `restricted` control it CAN, both at the pod AND the container securityContext
// level, so a regression that adds a privilege (drops the seccomp profile, flips
// privileged on, allows escalation, or stops dropping ALL capabilities) is caught
// here. It also pins the DOCUMENTED EXCEPTIONS so they cannot drift silently: the
// husk pod is admitted into a baseline/restricted namespace only EXCEPT the
// read-only snapshot hostPath (forbidden under both baseline and restricted) and
// runAsNonRoot=false (forbidden under restricted, the /dev/kvm device exception),
// plus the mitos.run/kvm device-plugin resource. The empirical PSA finding (a
// restricted namespace rejects the husk pod on exactly hostPath + runAsNonRoot,
// and the SAME securityContext minus those two is admitted into restricted) is
// proven object-level on kind in the conformance job; this unit test pins the
// spec fields those exceptions and the satisfied controls correspond to.
func TestBuildHuskPodPSARestricted(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "psa-pool", Namespace: "default", UID: "pool-uid-psa"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:  "mitos-husk-stub:test",
		SnapshotID: "psa-tmpl",
		DataDir:    "/var/lib/mitos",
	})

	// POD-LEVEL securityContext: PSA restricted checks seccompProfile at the pod
	// OR the container level; we set BOTH so the pod-level control is satisfied
	// even if a future container is added without its own profile.
	psc := pod.Spec.SecurityContext
	if psc == nil {
		t.Fatal("pod-level SecurityContext is nil; PSA restricted checks the pod-level seccompProfile")
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod SeccompProfile = %+v, want RuntimeDefault", psc.SeccompProfile)
	}
	// runAsNonRoot at the pod level is the documented exception: it is FALSE so
	// Firecracker can open the device-plugin-injected /dev/kvm as uid 0 WITHOUT
	// privileged. This is the ONLY restricted securityContext control the husk pod
	// does not satisfy; it is documented, not accidental.
	if psc.RunAsNonRoot == nil || *psc.RunAsNonRoot {
		t.Error("pod RunAsNonRoot must be explicitly false (the documented /dev/kvm device exception)")
	}

	// CONTAINER-LEVEL securityContext: every other restricted control IS satisfied,
	// so the husk pod's securityContext is restricted-clean and only the hostPath +
	// runAsNonRoot exceptions keep it out of a restricted namespace.
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container SecurityContext is nil")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("container Privileged must be explicitly false (restricted: privileged forbidden)")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("container AllowPrivilegeEscalation must be explicitly false (restricted control)")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container Capabilities.Drop = %+v, want [ALL] (restricted control)", sc.Capabilities)
	}
	if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "NET_ADMIN" {
		t.Errorf("container Capabilities.Add = %+v, want [NET_ADMIN] (documented PSA exception for in-pod firewalling)", sc.Capabilities.Add)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container SeccompProfile = %+v, want RuntimeDefault (restricted control)", sc.SeccompProfile)
	}

	// DOCUMENTED EXCEPTION: the read-only snapshot hostPath. It is forbidden under
	// BOTH baseline and restricted; the husk pod carries it as the documented
	// node-snapshot-read exception. Pin it read-only so a regression to a writable
	// snapshot mount is caught.
	var snapVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot" {
			snapVol = &pod.Spec.Volumes[i]
		}
	}
	if snapVol == nil || snapVol.HostPath == nil {
		t.Fatal("snapshot volume must be a hostPath (the documented node-snapshot exception)")
	}
	var snapMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == "snapshot" {
			snapMount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if snapMount == nil || !snapMount.ReadOnly {
		t.Errorf("snapshot mount = %+v, want present and ReadOnly", snapMount)
	}

	// DOCUMENTED EXCEPTION: the /dev/kvm device-plugin resource request (request
	// AND limit), which replaces privileged: true.
	kvm := corev1.ResourceName("mitos.run/kvm")
	if got := pod.Spec.Containers[0].Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("kvm request = %s, want 1 (the device-plugin exception)", got.String())
	}

	// production-blocker #2, cap 1: the memory LIMIT is set (host-DoS cap) and
	// strictly exceeds the request (headroom so a legitimate VM is never
	// OOM-killed). Setting a limit does not affect PSA admission (PSA does not
	// gate on resource limits), so this stays restricted-clean.
	ctr := pod.Spec.Containers[0]
	lim, ok := ctr.Resources.Limits[corev1.ResourceMemory]
	if !ok {
		t.Fatal("memory limit must be set (host-DoS cap, production-blocker #2)")
	}
	if req := ctr.Resources.Requests[corev1.ResourceMemory]; lim.Cmp(req) <= 0 {
		t.Errorf("memory limit %s must exceed request %s (headroom)", lim.String(), req.String())
	}
	// CPU LIMIT is now set (burst cap enforced by cgroup cpu.max); CPU REQUEST is
	// the low floor (50m) for dense node packing. Setting a cpu limit does not
	// affect PSA admission (PSA does not gate on resource limits or requests).
	if _, ok := ctr.Resources.Limits[corev1.ResourceCPU]; !ok {
		t.Error("cpu limit must be set (burst cap for CPU DoS bound)")
	}
}

func TestBuildHuskPodControlAndSnapshot(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ctl-pool", Namespace: "default", UID: "pool-uid-9"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:     "mitos-husk-stub:test",
		SnapshotID:    "ctl-tmpl",
		DataDir:       "/var/lib/mitos",
		TLSSecretName: "forkd-tls",
		CASecretName:  "mitos-ca",
	})

	ctr := pod.Spec.Containers[0]
	args := strings.Join(ctr.Args, " ")

	// mTLS network control: the control-listen port + the three TLS PEM args.
	if !strings.Contains(args, "--control-listen") {
		t.Errorf("args missing --control-listen: %v", ctr.Args)
	}
	// The in-pod sandbox HTTP API is served on the sandbox port so the endpoint
	// the claim advertises (podIP:9091) is reachable and token-gated.
	if !strings.Contains(args, "--sandbox-listen :9091") {
		t.Errorf("args missing --sandbox-listen :9091: %v", ctr.Args)
	}
	for _, flag := range []string{"--tls-cert", "--tls-key", "--tls-ca"} {
		if !strings.Contains(args, flag) {
			t.Errorf("args missing %s: %v", flag, ctr.Args)
		}
	}

	// The sandbox endpoint port is exposed as a container port so the claim's
	// Status.Endpoint (podIP:port) is reachable.
	var hasSandboxPort bool
	for _, p := range ctr.Ports {
		if p.ContainerPort == 9091 {
			hasSandboxPort = true
		}
	}
	if !hasSandboxPort {
		t.Errorf("container ports = %+v, want one with 9091 (sandbox endpoint)", ctr.Ports)
	}

	// A read-only mount of the node's template snapshot dir and the kernel, plus
	// the TLS Secret mount.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.Name] = m
	}
	if m, ok := mounts["snapshot"]; !ok || !m.ReadOnly {
		t.Errorf("snapshot mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["kernel"]; !ok || !m.ReadOnly {
		t.Errorf("kernel mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["husk-tls"]; !ok || !m.ReadOnly {
		t.Errorf("husk-tls mount missing or not read-only: %+v", mounts)
	}
	if m, ok := mounts["husk-ca"]; !ok || !m.ReadOnly {
		t.Errorf("husk-ca mount missing or not read-only: %+v", mounts)
	}

	// The snapshot hostPath points at <dataDir>/templates/<snapshotID>/snapshot.
	var snapVol *corev1.Volume
	var tlsVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case "snapshot":
			snapVol = &pod.Spec.Volumes[i]
		case "husk-tls":
			tlsVol = &pod.Spec.Volumes[i]
		}
	}
	if snapVol == nil || snapVol.HostPath == nil {
		t.Fatalf("snapshot volume is not a hostPath: %+v", snapVol)
	}
	if snapVol.HostPath.Path != "/var/lib/mitos/templates/ctl-tmpl/snapshot" {
		t.Errorf("snapshot hostPath = %q, want /var/lib/mitos/templates/ctl-tmpl/snapshot", snapVol.HostPath.Path)
	}
	if tlsVol == nil || tlsVol.Secret == nil || tlsVol.Secret.SecretName != "forkd-tls" {
		t.Errorf("husk-tls volume should mount the forkd-tls Secret: %+v", tlsVol)
	}

	// Placement: the pod must land on a KVM node.
	if pod.Spec.NodeSelector["mitos.run/kvm"] != "true" {
		t.Errorf("nodeSelector = %+v, want mitos.run/kvm=true", pod.Spec.NodeSelector)
	}
}

// TestBuildHuskPodMountsManifestWhenDigestKnown proves that when the pool has a
// recorded template digest, the husk pod mounts the recorded CAS manifest
// read-only and runs the stub with verify ENFORCED (--manifest, no escape
// hatch), so the stub re-verifies the snapshot before loading (fail-closed).
func TestBuildHuskPodMountsManifestWhenDigestKnown(t *testing.T) {
	const digest = "abc1230000000000000000000000000000000000000000000000000000000000"
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "verify-pool", Namespace: "default", UID: "pool-uid-v"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:      "mitos-husk-stub:test",
		SnapshotID:     "verify-tmpl",
		DataDir:        "/var/lib/mitos",
		ExpectedDigest: digest,
	})

	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	// The manifest is mounted as a DIRECTORY (Talos rejects strict single-file
	// hostPath checks at this depth); the stub is pointed at <dir>/<digest>.
	if !strings.Contains(args, "--manifest /var/lib/mitos/manifests/"+digest) {
		t.Errorf("args missing --manifest mount path: %v", pod.Spec.Containers[0].Args)
	}
	if strings.Contains(args, "--allow-unverified-snapshots") {
		t.Errorf("verify must be ENFORCED when a digest is known; escape hatch present: %v", pod.Spec.Containers[0].Args)
	}
	// The dormant pod verifies the snapshot at Prepare (off the activate hot
	// path), so the controller passes the snapshot dir + expected digest.
	if !strings.Contains(args, "--snapshot-dir /var/lib/mitos/snapshot") {
		t.Errorf("args missing --snapshot-dir for prepare-time verification: %v", pod.Spec.Containers[0].Args)
	}
	if !strings.Contains(args, "--expected-digest "+digest) {
		t.Errorf("args missing --expected-digest for prepare-time verification: %v", pod.Spec.Containers[0].Args)
	}

	// The manifest hostPath is the CAS manifests DIRECTORY (not the single file).
	var manVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot-manifest" {
			manVol = &pod.Spec.Volumes[i]
		}
	}
	if manVol == nil || manVol.HostPath == nil {
		t.Fatalf("snapshot-manifest volume is not a hostPath: %+v", manVol)
	}
	if manVol.HostPath.Path != "/var/lib/mitos/cas/manifests" {
		t.Errorf("manifest hostPath = %q, want /var/lib/mitos/cas/manifests", manVol.HostPath.Path)
	}
	var mounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "snapshot-manifest" {
			mounted = true
			if !m.ReadOnly {
				t.Error("manifest mount must be read-only")
			}
		}
	}
	if !mounted {
		t.Error("manifest volume is not mounted into the container")
	}
}

func TestBuildHuskPodMountsWritableRootfsCoWDir(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "cow-pool", Namespace: "default", UID: "pool-uid-cow"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:  "mitos-husk-stub:test",
		SnapshotID: "cow-tmpl",
		DataDir:    "/var/lib/mitos",
	})
	container := pod.Spec.Containers[0]

	// The CoW dir hostPath volume must be present and WRITABLE (ReadOnly false),
	// co-located under the node data dir as a sibling of templates.
	var cowMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == "husk-rootfs-cow" {
			cowMount = &container.VolumeMounts[i]
		}
	}
	if cowMount == nil {
		t.Fatal("expected a husk-rootfs-cow volume mount")
	}
	if cowMount.ReadOnly {
		t.Error("the rootfs CoW dir must be mounted read-write")
	}

	var cowVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "husk-rootfs-cow" {
			cowVol = &pod.Spec.Volumes[i]
		}
	}
	if cowVol == nil || cowVol.HostPath == nil {
		t.Fatal("expected a husk-rootfs-cow hostPath volume")
	}
	wantHostPath := filepath.Join("/var/lib/mitos", "husk-rootfs")
	if cowVol.HostPath.Path != wantHostPath {
		t.Errorf("CoW hostPath = %q, want %q (sibling of templates under the data dir)", cowVol.HostPath.Path, wantHostPath)
	}

	// The stub must be told where to clone from and to.
	args := strings.Join(container.Args, " ")
	if !strings.Contains(args, "--rootfs-cow-dir "+cowMount.MountPath) {
		t.Errorf("args missing --rootfs-cow-dir %s: %v", cowMount.MountPath, container.Args)
	}
	wantTemplateRootfs := filepath.Join("/var/lib/mitos", "templates", "cow-tmpl", "rootfs.ext4")
	if !strings.Contains(args, "--template-rootfs "+wantTemplateRootfs) {
		t.Errorf("args missing --template-rootfs %s: %v", wantTemplateRootfs, container.Args)
	}

	// The template dir mount is READ-WRITE: Firecracker opens the snapshot's baked
	// rootfs path (this template rootfs.ext4) with O_RDWR during /snapshot/load, so
	// a read-only mount makes the load fail EROFS (verified on real KVM). Isolation
	// is NOT from the mount mode: the VM stays paused through load -> PatchDrive
	// (rootfs -> per-pod clone) -> resume, so the guest writes only its clone, never
	// the template. The template is only opened (not written) during the paused load.
	var tmplMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == "template" {
			tmplMount = &container.VolumeMounts[i]
		}
	}
	if tmplMount == nil {
		t.Fatal("expected a template volume mount")
	}
	if tmplMount.ReadOnly {
		t.Error("the template dir mount must be read-write so Firecracker can open the baked rootfs path at load; isolation is from rebind-before-resume, not the mount mode")
	}

	// The per-pod VM id flows from the downward API pod name: a POD_NAME env from
	// metadata.name plus --vm-id $(POD_NAME). This scopes the clone path per pod so
	// two husk pods on one node never collide on the shared CoW hostPath.
	var podNameEnv *corev1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == "POD_NAME" {
			podNameEnv = &container.Env[i]
		}
	}
	if podNameEnv == nil {
		t.Fatal("expected a POD_NAME env var")
	}
	if podNameEnv.ValueFrom == nil || podNameEnv.ValueFrom.FieldRef == nil ||
		podNameEnv.ValueFrom.FieldRef.FieldPath != "metadata.name" {
		t.Errorf("POD_NAME must come from the downward API metadata.name, got %+v", podNameEnv.ValueFrom)
	}
	if !strings.Contains(args, "--vm-id $(POD_NAME)") {
		t.Errorf("args missing --vm-id $(POD_NAME): %v", container.Args)
	}
}

// TestBuildHuskPodEscapeHatchWhenNoDigest proves the fallback: with no recorded
// digest the husk pod mounts no manifest and runs the stub's development escape
// hatch so the warm pool still activates (the stub logs this loudly).
func TestBuildHuskPodEscapeHatchWhenNoDigest(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "nodigest-pool", Namespace: "default", UID: "pool-uid-n"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage:  "mitos-husk-stub:test",
		SnapshotID: "nodigest-tmpl",
		DataDir:    "/var/lib/mitos",
	})

	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--allow-unverified-snapshots") {
		t.Errorf("with no digest the stub must run the escape hatch: %v", pod.Spec.Containers[0].Args)
	}
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "snapshot-manifest" {
			t.Error("no manifest should be mounted when no digest is recorded")
		}
	}
}

func TestBuildHuskPodDefaultSizing(t *testing.T) {
	// A template with no Resources: the builder must fall back to the
	// documented default (1 cpu / 512Mi).
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "def-pool", Namespace: "default", UID: "pool-uid-2"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{})

	// Default kvm resource name when opts leaves it empty.
	kvm := corev1.ResourceName("mitos.run/kvm")
	if got := pod.Spec.Containers[0].Resources.Requests[kvm]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Errorf("default kvm request = %s, want 1", got.String())
	}
	// CPU REQUEST is the low floor (50m); CPU LIMIT is the default cap (250m).
	if got := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("default cpu request = %s, want 50m (overcommit floor)", got.String())
	}
	if got := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("250m")) != 0 {
		t.Errorf("default cpu limit = %s, want 250m (defaultHuskCPU)", got.String())
	}
	if got := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("default memory request = %s, want 512Mi", got.String())
	}
	// Default-sized VM (512Mi) still gets a memory limit with headroom: 512Mi +
	// max(256Mi, 25% of 512Mi=128Mi) = 512Mi + 256Mi = 768Mi.
	wantMemLimit := resource.MustParse("512Mi")
	wantMemLimit.Add(resource.MustParse("256Mi"))
	if got := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantMemLimit) != 0 {
		t.Errorf("default memory limit = %s, want %s (request + headroom)", got.String(), wantMemLimit.String())
	}
}

// TestBuildHuskPodMemoryLimitWithHeadroom covers production-blocker #2, cap 1:
// the husk container carries a memory LIMIT (today: requests only, "no hard
// limit", so a tenant VM can OOM the node). The limit is sized = memory request
// + headroom so a VM running at its configured RAM is never OOM-killed (the
// headroom covers the Firecracker VMM, the husk-stub, and CoW dirty-page slack),
// while a runaway is capped. The default headroom is max(256Mi, 25% of the
// request). cpu stays requests-only (a cpu limit would throttle and hurt the
// activate latency).
func TestBuildHuskPodMemoryLimitWithHeadroom(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "mem-pool", Namespace: "default", UID: "pool-uid-mem"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image:     "python:3.12-slim",
				Resources: v1.SandboxResources{Memory: resource.MustParse("1Gi")},
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{})
	ctr := pod.Spec.Containers[0]

	// request stays the configured 1Gi (scheduler truth).
	if got := ctr.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory request = %s, want 1Gi", got.String())
	}
	// limit = 1Gi + max(256Mi, 25% of 1Gi) = 1Gi + 256Mi = 1280Mi.
	wantLimit := resource.MustParse("1Gi")
	wantLimit.Add(resource.MustParse("256Mi"))
	if got := ctr.Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantLimit) != 0 {
		t.Errorf("memory limit = %s, want %s (request + 256Mi headroom)", got.String(), wantLimit.String())
	}
	// The limit must be STRICTLY GREATER than the request: a too-tight limit
	// (limit == request) OOM-kills the VM as soon as the VMM and CoW slack are
	// counted, destroying the activate latency. This is the load-bearing invariant.
	req := ctr.Resources.Requests[corev1.ResourceMemory]
	if lim := ctr.Resources.Limits[corev1.ResourceMemory]; lim.Cmp(req) <= 0 {
		t.Errorf("memory limit %s must exceed request %s (headroom for the VMM, stub, CoW slack)", lim.String(), req.String())
	}

	// CPU LIMIT is set (burst cap, enforced by cgroup cpu.max); CPU REQUEST is the
	// low floor (50m). Restore and activate run up to this limit (the configured
	// per-sandbox cap). QoS stays Burstable (requests < limits for cpu and memory).
	if _, ok := ctr.Resources.Limits[corev1.ResourceCPU]; !ok {
		t.Errorf("cpu limit must be set (burst cap for node CPU overcommit model); limits = %+v", ctr.Resources.Limits)
	}
}

// TestBuildHuskPodMemoryLimitProportionalForLargeVM verifies the percentage
// component dominates for a large VM: a 16Gi request gets 25% = 4Gi of headroom
// (not the 256Mi floor), so the absolute slack scales with the VM.
func TestBuildHuskPodMemoryLimitProportionalForLargeVM(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "big-pool", Namespace: "default", UID: "pool-uid-big"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image:     "python:3.12-slim",
				Resources: v1.SandboxResources{Memory: resource.MustParse("16Gi")},
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}

	c := k8sClient
	r := &controller.SandboxPoolReconciler{Client: c}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{})
	ctr := pod.Spec.Containers[0]

	// limit = 16Gi + max(256Mi, 25% of 16Gi=4Gi) = 16Gi + 4Gi = 20Gi.
	wantLimit := resource.MustParse("16Gi")
	wantLimit.Add(resource.MustParse("4Gi"))
	if got := ctr.Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantLimit) != 0 {
		t.Errorf("memory limit = %s, want %s (request + 25%% headroom)", got.String(), wantLimit.String())
	}
}

// TestBuildHuskPodMemoryLimitConfigurableHeadroom verifies an operator can tune
// the fixed-floor headroom via the reconciler field (the --husk-memory-headroom
// flag): a 512Mi floor produces request + 512Mi for a small VM where the floor
// dominates the percentage.
func TestBuildHuskPodMemoryLimitConfigurableHeadroom(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg-pool", Namespace: "default", UID: "pool-uid-cfg"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image:     "python:3.12-slim",
				Resources: v1.SandboxResources{Memory: resource.MustParse("512Mi")},
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}

	c := k8sClient
	headroom := resource.MustParse("512Mi")
	r := &controller.SandboxPoolReconciler{Client: c, HuskMemoryHeadroom: headroom}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{})
	ctr := pod.Spec.Containers[0]

	// limit = 512Mi + max(512Mi floor, 25% of 512Mi=128Mi) = 512Mi + 512Mi = 1Gi.
	if got := ctr.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("memory limit = %s, want 1Gi (request + 512Mi configured floor)", got.String())
	}
}

func listHuskPods(t *testing.T, c client.Client, poolName string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace("default"),
		client.MatchingLabels{"mitos.run/pool": poolName, "mitos.run/husk": "true"},
	); err != nil {
		t.Fatalf("list husk pods: %v", err)
	}
	return pods.Items
}

func waitHuskPodCount(t *testing.T, c client.Client, poolName string, want int) []corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []corev1.Pod
	for time.Now().Before(deadline) {
		last = listHuskPods(t, c, poolName)
		if len(last) == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("husk pod count for %s = %d, want %d", poolName, len(last), want)
	return nil
}

func TestReconcileHuskPodsCreateScaleAndFlagOff(t *testing.T) {
	c := k8sClient

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 3},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "husk-pool") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    controller.NewNodeRegistry(),
		EnableHuskPods:  true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}

	// Re-fetch the pool so the reconciler works against a server-populated UID
	// (SetControllerReference requires the owner UID).
	var got v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &got); err != nil {
		t.Fatal(err)
	}

	// Create: warm.min=3 -> 3 husk pod objects owned by the pool.
	count, err := r.ReconcileHuskPodsForTest(ctx, &got, got.Spec.Template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (create): %v", err)
	}
	if count != 3 {
		t.Fatalf("reconcileHuskPods returned %d, want 3", count)
	}
	pods := waitHuskPodCount(t, c, "husk-pool", 3)
	for _, p := range pods {
		owner := metav1.GetControllerOf(&p)
		if owner == nil || owner.Kind != "SandboxPool" || owner.Name != "husk-pool" {
			t.Fatalf("husk pod %s owner = %+v, want SandboxPool husk-pool", p.Name, owner)
		}
	}

	// Idempotent: a second reconcile at the same warm.min creates nothing new.
	count, err = r.ReconcileHuskPodsForTest(ctx, &got, got.Spec.Template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (idempotent): %v", err)
	}
	if count != 3 {
		t.Fatalf("idempotent reconcile returned %d, want 3", count)
	}
	waitHuskPodCount(t, c, "husk-pool", 3)

	// Scale down: warm.min=1 -> 2 deleted.
	got.Spec.Warm.Min = 1
	count, err = r.ReconcileHuskPodsForTest(ctx, &got, got.Spec.Template)
	if err != nil {
		t.Fatalf("reconcileHuskPods (scale down): %v", err)
	}
	if count != 1 {
		t.Fatalf("reconcileHuskPods after scale-down returned %d, want 1", count)
	}
	waitHuskPodCount(t, c, "husk-pool", 1)
}

func TestReconcileHuskPodsFlagOffCreatesNone(t *testing.T) {
	c := k8sClient

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "off-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 2},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Delete(ctx, pool)
	})

	// EnableHuskPods false: the pool reconcile runs the raw-forkd path through
	// the manager (no fake forkd node registered, so no snapshots either). The
	// invariant under test is that NO husk pods exist.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(listHuskPods(t, c, "off-pool")); n != 0 {
			t.Fatalf("husk pods created with flag off: %d", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestBuildHuskPodMountsForksDir(t *testing.T) {
	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	tmpl := &v1.PoolTemplateSpec{}
	pod := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", SnapshotID: "tmpl-a", DataDir: "/data"})

	var found bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "husk-forks" && v.HostPath != nil && v.HostPath.Path == "/data/forks" {
			found = true
		}
	}
	if !found {
		t.Fatalf("husk pod missing the read-write forks hostPath volume; volumes=%+v", pod.Spec.Volumes)
	}
}

func TestBuildHuskPodForkChildActivatesFromForkSnapshot(t *testing.T) {
	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	tmpl := &v1.PoolTemplateSpec{}
	pod := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "fork-1",
		ForkSourceNode: "kvm-node-1",
	})

	// The snapshot mount must point at the FORK snapshot dir, not the template's.
	var snapPath string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "snapshot" && v.HostPath != nil {
			snapPath = v.HostPath.Path
		}
	}
	if snapPath != "/data/forks/fork-1" {
		t.Fatalf("fork child snapshot mount = %q, want /data/forks/fork-1", snapPath)
	}
	// The child is pinned to the source node.
	if pod.Spec.Affinity == nil {
		t.Fatalf("fork child must be pinned to the source node via affinity")
	}
}

// TestBuildHuskPodForkChildClonesFromSourceRootfs is the BUG 1 regression: a
// fork child restores the SOURCE's live fork snapshot, whose vmstate was baked
// against the SOURCE's rootfs. Its per-activation CoW clone MUST therefore be
// sourced from the SOURCE pod's rootfs, not the pristine template rootfs;
// otherwise the child's guest memory (page cache, ext4 superblock) reflects the
// source disk while the block device is the template disk: silent divergence /
// fs corruption. The controller threads the source pod's in-pod rootfs path
// through ForkSourceRootfsPath; --template-rootfs (the clone SOURCE) must be it.
func TestBuildHuskPodForkChildClonesFromSourceRootfs(t *testing.T) {
	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	tmpl := &v1.PoolTemplateSpec{}
	srcRootfs := filepath.Join("/var/lib/mitos", "husk-rootfs", "src-pod-xyz", "rootfs.ext4")
	pod := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{
		StubImage:            "img",
		SnapshotID:           "tmpl-a",
		DataDir:              "/data",
		ForkSnapshotID:       "fork-1",
		ForkSourceNode:       "kvm-node-1",
		ForkSourceRootfsPath: srcRootfs,
	})

	var container corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "husk-stub" {
			container = pod.Spec.Containers[i]
		}
	}
	args := strings.Join(container.Args, " ")
	// The clone SOURCE must be the SOURCE pod's rootfs, not the template's.
	if !strings.Contains(args, "--template-rootfs "+srcRootfs) {
		t.Fatalf("fork child --template-rootfs must be the source rootfs %q; args=%v", srcRootfs, container.Args)
	}
	// And it must NOT clone from the pristine template rootfs (the bug).
	templateRootfs := filepath.Join("/data", "templates", "tmpl-a", "rootfs.ext4")
	if strings.Contains(args, "--template-rootfs "+templateRootfs) {
		t.Fatalf("fork child must NOT clone from the template rootfs %q (source/disk divergence); args=%v", templateRootfs, container.Args)
	}
	// Each child still gets its OWN per-activation CoW clone (independence):
	// the CoW DEST dir is still the per-pod writable dir.
	const cowMountPath = "/var/lib/mitos/husk-rootfs"
	if !strings.Contains(args, "--rootfs-cow-dir "+cowMountPath) {
		t.Errorf("fork child must still write its own per-activation clone under %q; args=%v", cowMountPath, container.Args)
	}
}

func TestBuildForkChildPodOwnedByFork(t *testing.T) {
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "f1", Namespace: "default", UID: "uid-f1"}}
	srcPod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "kvm-node-1"}}
	pod := controller.BuildForkChildPodForTest(fork, srcPod, "child-0", controller.HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "f1",
		ForkSourceNode: "kvm-node-1",
	}, scheme)

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.UID != "uid-f1" {
		t.Fatalf("fork child not owned by the SandboxFork: %+v", owner)
	}
	if pod.Labels["mitos.run/fork"] != "f1" {
		t.Fatalf("fork child missing fork label, labels=%+v", pod.Labels)
	}
	if _, ok := pod.Labels["mitos.run/pool"]; ok {
		t.Fatalf("fork child must NOT carry the pool warm-slot label, labels=%+v", pod.Labels)
	}
}

// TestBuildForkChildPodCarriesPoolCPUCap is the issue #760 regression for the
// resource half: a live-fork child built from an EMPTY PoolTemplateSpec lost the
// pool's cpu burst cap and ran at the default 250m ceiling instead of the
// tenant's configured cap. buildForkChildPod must thread the resolved source
// pool template's resources into the child pod, so the child's cpu LIMIT matches
// the pool cap a warm-claimed sandbox of the same pool gets.
func TestBuildForkChildPodCarriesPoolCPUCap(t *testing.T) {
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "cap-fork", Namespace: "default", UID: "uid-cap"}}
	srcPod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "kvm-node-1"}}
	poolCPU := resource.MustParse("2")
	child := controller.BuildForkChildPodForTest(fork, srcPod, "cap-child-0", controller.HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "cap-fork",
		ForkSourceNode: "kvm-node-1",
		// The resolved SOURCE pool template: the fork child must inherit its cpu cap.
		Template: &v1.PoolTemplateSpec{Resources: v1.SandboxResources{CPU: poolCPU}},
	}, scheme)

	var c corev1.Container
	for i := range child.Spec.Containers {
		if child.Spec.Containers[i].Name == "husk-stub" {
			c = child.Spec.Containers[i]
		}
	}
	got := c.Resources.Limits[corev1.ResourceCPU]
	if got.Cmp(poolCPU) != 0 {
		t.Fatalf("fork child cpu limit = %s, want the pool cap %s (not the default): the child lost the pool cpu burst cap", got.String(), poolCPU.String())
	}
}

// TestBuildHuskPodDisablesSATokenAutomount asserts the husk pod opts out of the
// default ServiceAccount token automount. The stub speaks vsock + mTLS and never
// calls the Kubernetes API, so mounting the namespace default SA token would only
// hand a guest that escaped into the stub a free system:authenticated token.
func TestBuildHuskPodDisablesSATokenAutomount(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "sa-pool", Namespace: "default", UID: "pool-uid-sa"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}

	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "img"})

	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("warm husk pod must set AutomountServiceAccountToken (got nil)")
	}
	if *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("warm husk pod AutomountServiceAccountToken = true, want false")
	}

	// Fork-child pods share buildHuskPod, so they must inherit the opt-out too.
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sa-fork", Namespace: "default", UID: "uid-sa-fork"}}
	srcPod := &corev1.Pod{Spec: corev1.PodSpec{NodeName: "kvm-node-1"}}
	child := controller.BuildForkChildPodForTest(fork, srcPod, "sa-child-0", controller.HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "sa-fork",
		ForkSourceNode: "kvm-node-1",
	}, scheme)
	if child.Spec.AutomountServiceAccountToken == nil || *child.Spec.AutomountServiceAccountToken {
		t.Errorf("fork-child husk pod AutomountServiceAccountToken = %v, want false", child.Spec.AutomountServiceAccountToken)
	}
}

// TestBuildHuskPodFastNodeLossEviction asserts husk pods carry a short NoExecute
// toleration (60s) for not-ready/unreachable instead of the Kubernetes default
// 300s, so a node loss evicts the pod and fails an active claim over (or refills
// the warm pool) in ~a minute, not ~5 (#177).
func TestBuildHuskPodFastNodeLossEviction(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "tol-pool", Namespace: "default", UID: "tol-uid"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "mitos-husk-stub:test", KVMResourceName: "mitos.run/kvm"})

	want := map[string]int64{"node.kubernetes.io/not-ready": 60, "node.kubernetes.io/unreachable": 60}
	got := map[string]int64{}
	for _, tol := range pod.Spec.Tolerations {
		if tol.Effect == corev1.TaintEffectNoExecute && tol.TolerationSeconds != nil {
			got[tol.Key] = *tol.TolerationSeconds
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("husk pod NoExecute toleration %s tolerationSeconds = %d, want %d (#177 fast node-loss eviction)", k, got[k], v)
		}
	}
}

// TestBuildHuskPodPlacement asserts a pool's spec.placement (dedicatedNodes #172)
// is threaded onto the husk pod: the nodeSelector is merged with the KVM label
// and the tolerations are appended to the node-loss tolerations.
func TestBuildHuskPodPlacement(t *testing.T) {
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ded-pool", Namespace: "default", UID: "ded-uid"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
			Placement: &v1.PoolPlacement{
				NodeSelector: map[string]string{"mitos.run/tenant": "acme"},
				Tolerations:  []corev1.Toleration{{Key: "mitos.run/tenant", Operator: corev1.TolerationOpEqual, Value: "acme", Effect: corev1.TaintEffectNoSchedule}},
			},
		},
	}
	r := &controller.SandboxPoolReconciler{Client: k8sClient}
	pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{
		StubImage: "mitos-husk-stub:test", KVMResourceName: "mitos.run/kvm",
		PlacementNodeSelector: pool.Spec.Placement.NodeSelector,
		PlacementTolerations:  pool.Spec.Placement.Tolerations,
	})
	if pod.Spec.NodeSelector["mitos.run/kvm"] != "true" || pod.Spec.NodeSelector["mitos.run/tenant"] != "acme" {
		t.Errorf("nodeSelector = %v, want kvm=true + tenant=acme", pod.Spec.NodeSelector)
	}
	found := false
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == "mitos.run/tenant" && tol.Value == "acme" {
			found = true
		}
	}
	if !found {
		t.Errorf("placement toleration mitos.run/tenant=acme not appended: %v", pod.Spec.Tolerations)
	}
}

// TestBuildHuskPodStampsOrgFromNamespace is the billing trust boundary (issue
// #164). The org label on a husk pod is the metering attribution key, so it MUST
// be derived from the TRUSTED per-org namespace the control plane placed the pool
// in (mitos-org-<id>), never from client input. The cases prove:
//   - a pool in mitos-org-acme yields a husk pod labeled mitos.run/org=acme;
//   - a pool in a non-org namespace (self-host single-tenant) carries NO org
//     label (it stays unattributed rather than being forced into a bogus org);
//   - a CLIENT-SET mitos.run/org on the input pool is IGNORED: the controller
//     overwrites it with the namespace-derived org, so a tenant cannot bill
//     another org by setting the label.
func TestBuildHuskPodStampsOrgFromNamespace(t *testing.T) {
	const orgLabel = "mitos.run/org"

	// A pool builder with no Client: buildHuskPod skips the pool owner ref when
	// Client is nil, so the org-label stamp is exercised without a live cluster.
	r := &controller.SandboxPoolReconciler{}

	t.Run("org namespace stamps the namespace-derived org", func(t *testing.T) {
		pool := &v1.SandboxPool{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "mitos-org-acme"},
			Spec:       v1.SandboxPoolSpec{Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"}},
		}
		pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "img"})
		if got := pod.Labels[orgLabel]; got != "acme" {
			t.Errorf("org label = %q, want acme (derived from namespace mitos-org-acme)", got)
		}
	})

	t.Run("non-org namespace leaves the pod unattributed", func(t *testing.T) {
		pool := &v1.SandboxPool{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
			Spec:       v1.SandboxPoolSpec{Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"}},
		}
		pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "img"})
		if got, ok := pod.Labels[orgLabel]; ok {
			t.Errorf("org label = %q, want NO org label for a self-host non-org namespace", got)
		}
	})

	t.Run("client-set org label is ignored (trust boundary)", func(t *testing.T) {
		// The input pool carries an attacker-controlled org label; the namespace
		// is mitos-org-acme, so the pod MUST be attributed to acme, not evil.
		pool := &v1.SandboxPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p",
				Namespace: "mitos-org-acme",
				Labels:    map[string]string{orgLabel: "evil"},
			},
			Spec: v1.SandboxPoolSpec{Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"}},
		}
		pod := r.BuildHuskPodForTest(pool, pool.Spec.Template, controller.HuskPodOptions{StubImage: "img"})
		if got := pod.Labels[orgLabel]; got != "acme" {
			t.Errorf("org label = %q, want acme (client-set %q must be ignored)", got, "evil")
		}
	})
}

// TestBuildForkChildPodInheritsSourcePodScheduling is the builder-level
// regression for the production fork 504 (FailedScheduling: "1 node(s) had
// untolerated taint {mitos.run/dedicated}"). buildForkChildPod pins the child
// to the source node via nodeAffinity, but affinity does not clear taints: the
// child must also inherit the SOURCE pod's tolerations and nodeSelector (the
// pod spec is the authoritative record of what it took to schedule onto that
// node; the pool's spec.placement may have changed since). The fast node-loss
// pair huskTolerations adds to every husk pod must not be duplicated by the
// inheritance. Only a real-cluster e2e proves scheduling against actual taints
// (kind/envtest nodes are untainted); this pins the emitted spec.
func TestBuildForkChildPodInheritsSourcePodScheduling(t *testing.T) {
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "f-sched", Namespace: "default", UID: "uid-f-sched"}}
	notReadySecs := int64(60)
	srcPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a-husk-abcde", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "kvm-node-1",
			NodeSelector: map[string]string{
				"mitos.run/kvm":    "true",
				"mitos.run/tenant": "acme",
			},
			Tolerations: []corev1.Toleration{
				{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReadySecs},
				{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReadySecs},
				{Key: "mitos.run/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			},
		},
	}

	child := controller.BuildForkChildPodForTest(fork, srcPod, "f-sched-fork-0", controller.HuskPodOptions{
		StubImage:      "img",
		SnapshotID:     "tmpl-a",
		DataDir:        "/data",
		ForkSnapshotID: "f-sched",
		ForkSourceNode: srcPod.Spec.NodeName,
	}, scheme)

	tolCount := map[string]int{}
	for _, tol := range child.Spec.Tolerations {
		tolCount[tol.Key]++
	}
	if tolCount["mitos.run/dedicated"] != 1 {
		t.Errorf("fork child must inherit the source pod's mitos.run/dedicated toleration exactly once, got %d; tolerations=%v", tolCount["mitos.run/dedicated"], child.Spec.Tolerations)
	}
	for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		if tolCount[key] != 1 {
			t.Errorf("fork child toleration %s count = %d, want exactly 1 (huskTolerations adds it; inheritance must not duplicate it)", key, tolCount[key])
		}
	}
	if child.Spec.NodeSelector["mitos.run/tenant"] != "acme" || child.Spec.NodeSelector["mitos.run/kvm"] != "true" {
		t.Errorf("fork child nodeSelector = %v, want the source pod's kvm=true + tenant=acme merged in", child.Spec.NodeSelector)
	}
	// The exact-node affinity pin must survive the inheritance: the fork
	// snapshot and source rootfs are node-local hostPaths on kvm-node-1.
	aff := child.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("fork child must keep the required nodeAffinity source-node pin; affinity=%v", aff)
	}
	pinned := false
	for _, term := range aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" && len(expr.Values) == 1 && expr.Values[0] == "kvm-node-1" {
				pinned = true
			}
		}
	}
	if !pinned {
		t.Errorf("fork child nodeAffinity must pin to the source node kvm-node-1; affinity=%v", aff.NodeAffinity)
	}
}

// TestReconcileHuskPodsThreadsMultiVM proves the r.MultiVM to opts.MultiVM
// threading in reconcileHuskPods (L1.7d): a pool reconciled by a MultiVM-enabled
// reconciler creates warm husk pods that carry the mitos.run/multi-vm capability
// label, and a MultiVM-off reconciler creates pods without it. This guards the
// threading line that TestBuildHuskPodMultiVMArgAndLabel (a direct buildHuskPod
// call) cannot.
func TestReconcileHuskPodsThreadsMultiVM(t *testing.T) {
	c := k8sClient
	const label = "mitos.run/multi-vm"

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-pool-mvm", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "husk-pool-mvm") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, pool)
	})
	var got v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &got); err != nil {
		t.Fatal(err)
	}

	r := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    controller.NewNodeRegistry(),
		EnableHuskPods:  true,
		MultiVM:         true,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}
	if _, err := r.ReconcileHuskPodsForTest(ctx, &got, got.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (multi-vm): %v", err)
	}
	pods := waitHuskPodCount(t, c, "husk-pool-mvm", 1)
	for _, p := range pods {
		if p.Labels[label] != "true" {
			t.Fatalf("MultiVM reconciler must stamp %s=true on warm husk pods, got labels %v", label, p.Labels)
		}
	}

	// Negative case: a MultiVM-OFF reconciler must NOT stamp the label. This
	// catches an inverted-condition regression (label always applied) that the
	// positive case alone would miss.
	offPool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "husk-pool-mvm-off", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{Image: "python:3.12-slim"},
			Warm:     &v1.PoolWarm{Min: 1},
		},
	}
	if err := c.Create(ctx, offPool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range listHuskPods(t, c, "husk-pool-mvm-off") {
			_ = c.Delete(ctx, &p)
		}
		_ = c.Delete(ctx, offPool)
	})
	var gotOff v1.SandboxPool
	if err := c.Get(ctx, client.ObjectKeyFromObject(offPool), &gotOff); err != nil {
		t.Fatal(err)
	}
	rOff := &controller.SandboxPoolReconciler{
		Client:          c,
		NodeRegistry:    controller.NewNodeRegistry(),
		EnableHuskPods:  true,
		MultiVM:         false,
		HuskStubImage:   "mitos-husk-stub:test",
		KVMResourceName: "mitos.run/kvm",
	}
	if _, err := rOff.ReconcileHuskPodsForTest(ctx, &gotOff, gotOff.Spec.Template); err != nil {
		t.Fatalf("reconcileHuskPods (multi-vm off): %v", err)
	}
	for _, p := range waitHuskPodCount(t, c, "husk-pool-mvm-off", 1) {
		if _, ok := p.Labels[label]; ok {
			t.Fatalf("MultiVM-off reconciler must NOT stamp %s, got labels %v", label, p.Labels)
		}
	}
}

// TestBuildHuskPodThreadsPrepareEgressLink proves the prepare-time link flag reaches the
// stub together with the in-pod addresses it cannot work without: the tap name derives
// from the guest IP, and the stub needs it BEFORE an activate request arrives. Off (the
// default) the pod is byte-for-byte the current activate-time filter.
func TestBuildHuskPodThreadsPrepareEgressLink(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	on := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", MultiVM: true, PrepareEgressLink: true})
	args := on.Spec.Containers[0].Args
	for _, want := range []string{"--prepare-egress-link", "--in-pod-guest-ip", "--in-pod-gateway-ip"} {
		if !argsContain(args, want) {
			t.Errorf("husk args missing %s when prepare-egress-link is enabled: %v", want, args)
		}
	}
	// The addresses must be the ones the activate request carries, or the stub would
	// bring up a tap the claim never resolves to and the skip would silently not apply.
	if !argsContain(args, "10.200.0.2") || !argsContain(args, "10.200.0.1") {
		t.Errorf("husk args do not carry the fixed in-pod /30: %v", args)
	}

	off := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{StubImage: "img", MultiVM: true})
	for _, unwanted := range []string{"--prepare-egress-link", "--in-pod-guest-ip", "--in-pod-gateway-ip"} {
		if argsContain(off.Spec.Containers[0].Args, unwanted) {
			t.Errorf("husk args must omit %s by default: %v", unwanted, off.Spec.Containers[0].Args)
		}
	}
}

// TestBuildHuskPodOmitsPrepareEgressLinkWithoutMultiVM: the option is documented as
// requiring MultiVM, and the stub only brings its tap up on the multi-VM Prepare path.
// Emitting the flags to a single-VM stub would produce an unsupported pod shape whose
// arguments do nothing. cmd/controller rejects the combination outright; this pins the
// second gate, at the boundary that actually builds the pod.
func TestBuildHuskPodOmitsPrepareEgressLinkWithoutMultiVM(t *testing.T) {
	r := &controller.SandboxPoolReconciler{}
	pool := &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	tmpl := &v1.PoolTemplateSpec{}

	pod := r.BuildHuskPodForTest(pool, tmpl, controller.HuskPodOptions{
		StubImage: "img", MultiVM: false, PrepareEgressLink: true,
	})
	for _, unwanted := range []string{"--prepare-egress-link", "--in-pod-guest-ip", "--in-pod-gateway-ip"} {
		if argsContain(pod.Spec.Containers[0].Args, unwanted) {
			t.Errorf("husk args carry %s without --multi-vm: %v", unwanted, pod.Spec.Containers[0].Args)
		}
	}
}
