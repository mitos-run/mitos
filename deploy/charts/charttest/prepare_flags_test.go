package charttest

import (
	"os/exec"
	"strings"
	"testing"
)

// Prepare-time husk flags (the prepare-time-restore plan, issues #889/#871).
// The controller grew --husk-prepare-egress-link, --husk-prepare-restore, and
// --husk-prepare-kernel-prefault, but the chart never rendered them, so neither
// the hosted canary nor a self-hoster could enable the warm-claim latency work
// through values. These tests pin the wiring: OFF by default, rendered when
// set, and the dependency chain enforced at template time (the controller
// would refuse the same combinations at startup; failing the render is the
// operator-friendly earlier error).

// renderExpectTemplateFail asserts `helm template` fails with the given
// fragment in its output (a chart `fail` guard, not a schema violation).
func renderExpectTemplateFail(t *testing.T, fragment string, sets ...string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; skipping chart render test")
	}
	args := []string{"template", "t", chartDir(t), "--kube-version", "1.31.0"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil {
		t.Fatalf("helm template succeeded with %v; want the %q guard to fail the render", sets, fragment)
	}
	if !strings.Contains(string(out), fragment) {
		t.Fatalf("helm template failed but not on the expected guard %q:\n%s", fragment, out)
	}
}

// TestHuskPrepareFlagsOffByDefault: a default render carries none of the
// prepare-time flags; the warm-claim path is byte-for-byte the activate-time
// one for anyone who has not opted in.
func TestHuskPrepareFlagsOffByDefault(t *testing.T) {
	out := render(t)
	for _, needle := range []string{
		"--husk-prepare-egress-link",
		"--husk-prepare-restore",
		"--husk-prepare-kernel-prefault",
	} {
		if strings.Contains(out, needle) {
			t.Fatalf("%q rendered by default; the prepare-time flags must be opt-in", needle)
		}
	}
}

// TestHuskPrepareFlagsRenderWhenEnabled: with the full chain opted in, the
// controller container carries all three flags.
func TestHuskPrepareFlagsRenderWhenEnabled(t *testing.T) {
	dep := controllerDeployment(t, render(t,
		"controller.multiVMFork=true",
		"controller.huskPrepareEgressLink=true",
		"controller.huskPrepareRestore=true",
		"controller.huskPrepareKernelPrefault=true",
	))
	for _, needle := range []string{
		"- --husk-prepare-egress-link",
		"- --husk-prepare-restore",
		"- --husk-prepare-kernel-prefault",
	} {
		if !strings.Contains(dep, needle) {
			t.Fatalf("controller Deployment missing %q when the prepare chain is enabled", needle)
		}
	}
}

// TestHuskPrepareEgressLinkRequiresMultiVMFork: the tap is prepared only by a
// multi-VM husk pod, so the render fails fast rather than deploying a
// controller that refuses to start.
func TestHuskPrepareEgressLinkRequiresMultiVMFork(t *testing.T) {
	renderExpectTemplateFail(t, "controller.huskPrepareEgressLink requires controller.multiVMFork",
		"controller.huskPrepareEgressLink=true")
}

// TestHuskPrepareRestoreRequiresEgressLink: Firecracker binds the baked NIC to
// the prepared tap at snapshot load, so the restore cannot render without it.
func TestHuskPrepareRestoreRequiresEgressLink(t *testing.T) {
	renderExpectTemplateFail(t, "controller.huskPrepareRestore requires controller.huskPrepareEgressLink",
		"controller.multiVMFork=true",
		"controller.huskPrepareRestore=true")
}

// TestHuskPrepareKernelPrefaultRequiresRestore: there is no running guest to
// warm without the dormant restore.
func TestHuskPrepareKernelPrefaultRequiresRestore(t *testing.T) {
	renderExpectTemplateFail(t, "controller.huskPrepareKernelPrefault requires controller.huskPrepareRestore",
		"controller.multiVMFork=true",
		"controller.huskPrepareEgressLink=true",
		"controller.huskPrepareKernelPrefault=true")
}
