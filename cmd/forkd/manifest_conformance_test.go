package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// loadForkdDaemonSet parses the SHIPPED forkd DaemonSet manifest and returns its
// single container. The manifest is plain YAML (no Helm templating), so it can be
// asserted directly here; the Helm chart variant is conformance-checked against
// the same invariants by rendering in CI.
func loadForkdDaemonSet(t *testing.T, path string) corev1.Container {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var ds appsv1.DaemonSet
	if err := yaml.Unmarshal(raw, &ds); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	containers := ds.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected exactly one container in %s, got %d", path, len(containers))
	}
	return containers[0]
}

// k8sCapNames strips the CAP_ prefix from forkdRequiredCapabilities so the list
// can be compared against the Kubernetes capability spelling (Kubernetes writes
// capabilities without the CAP_ prefix, e.g. SYS_ADMIN).
func k8sCapNames() []string {
	caps := forkdRequiredCapabilities()
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = strings.TrimPrefix(c, "CAP_")
	}
	return out
}

// TestShippedDaemonSetIsNotPrivileged is the steady-state guard for issue #352:
// the shipped forkd DaemonSet must NOT run privileged: true. A regression that
// re-adds privileged: true (the worst shape for the most security-sensitive host
// component) fails this test in the darwin unit suite, before any KVM CI.
func TestShippedDaemonSetIsNotPrivileged(t *testing.T) {
	c := loadForkdDaemonSet(t, "../../deploy/daemon/daemonset.yaml")
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("forkd container has no securityContext; it must set the explicit jailer capability set")
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Fatal("forkd DaemonSet runs privileged: true; #352 requires the explicit jailer capability set instead")
	}
}

// TestShippedDaemonSetHasExactJailerCapabilities asserts the forkd container
// drops ALL capabilities and adds back EXACTLY the jailer set
// (jailerRequiredCapabilities), nothing more. The cap list is the single source
// of truth shared with buildJailerConfig's error message, so a widening of the
// granted set without updating the audited list is caught here.
func TestShippedDaemonSetHasExactJailerCapabilities(t *testing.T) {
	c := loadForkdDaemonSet(t, "../../deploy/daemon/daemonset.yaml")
	sc := c.SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatal("forkd container must drop ALL and add the explicit jailer capabilities")
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities.drop = %v, want [ALL]", sc.Capabilities.Drop)
	}
	got := make([]string, len(sc.Capabilities.Add))
	for i, c := range sc.Capabilities.Add {
		got[i] = string(c)
	}
	want := k8sCapNames()
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(got, ",") != strings.Join(sortedWant, ",") {
		t.Fatalf("capabilities.add = %v, want exactly %v (forkdRequiredCapabilities, CAP_ prefix stripped)", got, want)
	}
}

// TestShippedDaemonSetHardensSecurityContext asserts the non-privileged hardening
// that accompanies dropping privileged: no privilege escalation and an explicit
// RuntimeDefault seccomp profile (privileged: true had disabled seccomp; once it
// is gone the profile must be set explicitly, mirroring the husk pod).
func TestShippedDaemonSetHardensSecurityContext(t *testing.T) {
	c := loadForkdDaemonSet(t, "../../deploy/daemon/daemonset.yaml")
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("forkd container has no securityContext")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatal("forkd container must set allowPrivilegeEscalation: false")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("forkd container seccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
	}
}

// TestShippedDaemonSetGetsKVMViaDevicePlugin asserts forkd requests /dev/kvm and
// /dev/net/tun through the device plugin extended resource (mitos.run/kvm)
// instead of a privileged hostPath CharDevice. The device plugin sets the device
// cgroup allow that a non-privileged container otherwise lacks, so this request
// is what makes the privileged hostPath unnecessary.
func TestShippedDaemonSetGetsKVMViaDevicePlugin(t *testing.T) {
	c := loadForkdDaemonSet(t, "../../deploy/daemon/daemonset.yaml")
	const kvmResource = "mitos.run/kvm"
	q, ok := c.Resources.Limits[corev1.ResourceName(kvmResource)]
	if !ok {
		t.Fatalf("forkd container does not request the %s device-plugin resource; it is required to access /dev/kvm and /dev/net/tun without privileged", kvmResource)
	}
	if q.Value() != 1 {
		t.Fatalf("forkd container requests %s: %d, want 1", kvmResource, q.Value())
	}
	// The privileged /dev/kvm hostPath must be gone: device access now comes from
	// the plugin, not a host device node bind.
	for _, vm := range c.VolumeMounts {
		if vm.MountPath == "/dev/kvm" {
			t.Fatalf("forkd container still mounts /dev/kvm as a hostPath volume %q; remove it in favor of the device plugin", vm.Name)
		}
	}
}

// TestShippedDaemonSetEnablesJailer asserts the forkd args turn the jailer ON
// (the per-VM uid/gid, chroot, and cgroup). Without these flags the engine runs
// Firecracker unjailed and the capability drop is meaningless, so a regression
// that drops the flags is caught here.
func TestShippedDaemonSetEnablesJailer(t *testing.T) {
	c := loadForkdDaemonSet(t, "../../deploy/daemon/daemonset.yaml")
	joined := strings.Join(c.Args, "\n")
	for _, want := range []string{"--jailer=", "--chroot-base=", "--uid-range="} {
		if !strings.Contains(joined, want) {
			t.Errorf("forkd args missing %q; the jailer must be enabled for the capability drop to be meaningful. args:\n%s", want, joined)
		}
	}
}
