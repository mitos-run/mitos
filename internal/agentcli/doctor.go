package agentcli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// doctor implements the `mitos doctor` preflight (issue #174). It runs a set of
// CHECKS, each a small pure function over an injectable DoctorProbe, and prints
// a report whose every failing check carries an actionable, LLM-legible
// remediation in the issue #28 style. The command exits non-zero if any check
// fails so it composes in an install pipeline.
//
// Security: the probe surface answers PRESENCE booleans and counts only, never a
// secret value. The PKI and pull-secret checks ask "is this Secret present", not
// "what is in it", so a report can never leak a key, certificate, or token. This
// upholds the project rule that secret values are never logged.

// CheckStatus is the outcome of one preflight check.
type CheckStatus string

const (
	// CheckPass: the requirement is satisfied.
	CheckPass CheckStatus = "pass"
	// CheckWarn: the requirement is not satisfied but is non-blocking (e.g. a
	// private-registry pull secret that public images do not need). A warn never
	// flips the run to a non-zero exit on its own.
	CheckWarn CheckStatus = "warn"
	// CheckFail: a hard requirement is unmet; the install will not work until it
	// is fixed. Any fail makes the command exit non-zero.
	CheckFail CheckStatus = "fail"
)

// CheckResult is the outcome of one check: its stable name, status, a one-line
// detail naming what was observed (presence/counts only, never a value), and an
// actionable remediation that is set on warn/fail and empty on pass.
type CheckResult struct {
	Name        string
	Status      CheckStatus
	Detail      string
	Remediation string
}

// DoctorProbe is the thin platform seam the checks read through. Its methods are
// the only place that touches /dev, /proc, /sys, or the Kubernetes API; the
// check functions themselves are pure over this interface so they are fully
// unit-testable on any OS with a fake. The real implementation
// (linuxNodeProbe + k8sProbe) is the platform layer; some node reads are only
// meaningful on a Linux KVM node.
type DoctorProbe interface {
	// KVMDevice reports whether /dev/kvm is present and usable as a character
	// device on this node.
	KVMDevice(ctx context.Context) (bool, error)
	// KernelModuleLoaded reports whether the named kernel module is loaded (or
	// built in). name is one of nf_tables, vhost_vsock, tun.
	KernelModuleLoaded(ctx context.Context, name string) (bool, error)
	// GuestKernelStaged reports whether the guest kernel image is staged at the
	// path forkd boots from, and returns that path for the remediation message.
	GuestKernelStaged(ctx context.Context) (present bool, path string, err error)
	// PKISecretPresent reports whether the named PKI Secret exists in the install
	// namespace. It reads presence only, never the Secret contents.
	PKISecretPresent(ctx context.Context, name string) (bool, error)
	// ImagePullSecretPresent reports whether an image pull Secret is configured
	// in the install namespace. Presence only.
	ImagePullSecretPresent(ctx context.Context) (bool, error)
	// PoolNamespacePSA reports whether the pool namespace carries the privileged
	// Pod Security Admission enforce label, returning the observed label value
	// for the report.
	PoolNamespacePSA(ctx context.Context) (privileged bool, label string, err error)
	// Namespace is the install namespace the k8s checks ran against, for the
	// report and remediation text.
	Namespace() string
}

// requiredKernelModules is the set every KVM node must load. nf_tables is first
// because its absence breaks two things at once (see its check).
var requiredKernelModules = []string{"nf_tables", "vhost_vsock", "tun"}

// pkiSecretNames are the three Secrets the controller's EnsurePKI routine mints.
var pkiSecretNames = []string{"mitos-ca", "mitos-forkd-tls", "mitos-controller-tls"}

// RunDoctorChecks runs every preflight check against probe and returns the
// results in a stable order. It never returns an error: a probe failure is
// folded into the relevant check as a CheckFail with the probe error in the
// detail, so the report is always complete.
func RunDoctorChecks(ctx context.Context, probe DoctorProbe) []CheckResult {
	results := []CheckResult{checkKVM(ctx, probe)}
	for _, m := range requiredKernelModules {
		results = append(results, checkKernelModule(ctx, probe, m))
	}
	results = append(results,
		checkGuestKernel(ctx, probe),
		checkPKI(ctx, probe),
		checkPullSecret(ctx, probe),
		checkPSA(ctx, probe),
	)
	return results
}

func checkKVM(ctx context.Context, probe DoctorProbe) CheckResult {
	present, err := probe.KVMDevice(ctx)
	if err != nil {
		return CheckResult{
			Name:        "kvm-device",
			Status:      CheckFail,
			Detail:      fmt.Sprintf("could not read /dev/kvm: %v", err),
			Remediation: "Run mitos doctor on a KVM worker node. /dev/kvm must be a present, usable character device; on Hetzner use AX bare-metal (Cloud does not expose KVM), and ensure the kvm + kvm_intel/kvm_amd modules load at boot.",
		}
	}
	if !present {
		return CheckResult{
			Name:        "kvm-device",
			Status:      CheckFail,
			Detail:      "/dev/kvm is absent or not a character device",
			Remediation: "/dev/kvm is missing. Use a node with CPU virtualization (VT-x/AMD-V) exposed to the OS (Hetzner AX bare-metal, not Cloud) and load the kvm + kvm_intel/kvm_amd modules at boot. See docs/platforms/host-prerequisites.md.",
		}
	}
	return CheckResult{Name: "kvm-device", Status: CheckPass, Detail: "/dev/kvm present"}
}

func checkKernelModule(ctx context.Context, probe DoctorProbe, name string) CheckResult {
	loaded, err := probe.KernelModuleLoaded(ctx, name)
	res := CheckResult{Name: "kernel-module-" + name}
	if err != nil {
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("could not read module state: %v", err)
		res.Remediation = moduleRemediation(name)
		return res
	}
	if !loaded {
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("module %s is not loaded", name)
		res.Remediation = moduleRemediation(name)
		return res
	}
	res.Status = CheckPass
	res.Detail = fmt.Sprintf("module %s loaded", name)
	return res
}

// moduleRemediation returns the per-module fix. nf_tables names BOTH failure
// modes from issue #174 (husk egress isolation cannot run AND kube-proxy
// crash-loops) so an operator understands why a rescue/minimal kernel breaks two
// things at once.
func moduleRemediation(name string) string {
	const load = "Load it at boot (e.g. add it to /etc/modules-load.d/mitos.conf, or the Talos machine.kernel.modules list) and reboot the node."
	switch name {
	case "nf_tables":
		return "nf_tables is missing. Without it the husk egress isolation filter cannot run AND kube-proxy crash-loops, so this node cannot serve sandboxes or cluster networking. " + load + " A rescue/minimal kernel commonly lacks it; install a real OS. See docs/platforms/host-prerequisites.md."
	case "vhost_vsock":
		return "vhost_vsock is missing; the guest agent talks to forkd over vsock, so exec and file IO will not work without it. " + load
	case "tun":
		return "tun is missing; forkd creates a per-sandbox tap for guest networking, which fails without it. " + load
	default:
		return "Required kernel module " + name + " is missing. " + load
	}
}

func checkGuestKernel(ctx context.Context, probe DoctorProbe) CheckResult {
	present, path, err := probe.GuestKernelStaged(ctx)
	res := CheckResult{Name: "guest-kernel"}
	if path == "" {
		path = "/var/lib/mitos/vmlinux"
	}
	remediation := fmt.Sprintf("Guest kernel missing at %s; is the kernel-provisioner (the kernel-stage DaemonSet) healthy? Check kubectl -n mitos get pods -l app.kubernetes.io/component=kernel-stage and that the data dir is a writable real filesystem. forkd boots every microVM from this image and will not start without it.", path)
	if err != nil {
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("could not stat guest kernel at %s: %v", path, err)
		res.Remediation = remediation
		return res
	}
	if !present {
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("guest kernel not staged at %s", path)
		res.Remediation = remediation
		return res
	}
	res.Status = CheckPass
	res.Detail = fmt.Sprintf("guest kernel staged at %s", path)
	return res
}

func checkPKI(ctx context.Context, probe DoctorProbe) CheckResult {
	res := CheckResult{Name: "pki-secrets"}
	var missing []string
	for _, name := range pkiSecretNames {
		present, err := probe.PKISecretPresent(ctx, name)
		if err != nil {
			res.Status = CheckFail
			res.Detail = fmt.Sprintf("could not check PKI secret %s: %v", name, err)
			res.Remediation = pkiRemediation(probe.Namespace())
			return res
		}
		if !present {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		res.Status = CheckFail
		// Detail names the MISSING secrets by name only; their contents are never
		// read or printed.
		res.Detail = fmt.Sprintf("%d of %d PKI secrets missing: %s", len(missing), len(pkiSecretNames), strings.Join(missing, ", "))
		res.Remediation = pkiRemediation(probe.Namespace())
		return res
	}
	res.Status = CheckPass
	res.Detail = fmt.Sprintf("all %d PKI secrets present", len(pkiSecretNames))
	return res
}

func pkiRemediation(ns string) string {
	return fmt.Sprintf("The controller's EnsurePKI routine mints %s in namespace %s at startup. If they are missing, the controller has not run PKI bootstrap: check kubectl -n %s get deploy/mitos-controller is Running and inspect its logs. forkd mTLS depends on these.", strings.Join(pkiSecretNames, ", "), ns, ns)
}

func checkPullSecret(ctx context.Context, probe DoctorProbe) CheckResult {
	res := CheckResult{Name: "image-pull-secret"}
	present, err := probe.ImagePullSecretPresent(ctx)
	if err != nil {
		// A probe error here is a warn, not a fail: the pull secret is only needed
		// for private registries, so we do not block the run on an inability to
		// confirm it.
		res.Status = CheckWarn
		res.Detail = fmt.Sprintf("could not check image pull secret: %v", err)
		res.Remediation = pullSecretRemediation(probe.Namespace())
		return res
	}
	if !present {
		res.Status = CheckWarn
		res.Detail = "no image pull secret configured"
		res.Remediation = pullSecretRemediation(probe.Namespace())
		return res
	}
	res.Status = CheckPass
	res.Detail = "image pull secret present"
	return res
}

func pullSecretRemediation(ns string) string {
	return fmt.Sprintf("No image pull secret found in namespace %s. Public template images still pull, so this is non-blocking; if you pull from a PRIVATE registry, create a docker-registry Secret and reference it from the controller/forkd service accounts. See docs/platforms/host-prerequisites.md.", ns)
}

func checkPSA(ctx context.Context, probe DoctorProbe) CheckResult {
	res := CheckResult{Name: "psa-privileged"}
	privileged, label, err := probe.PoolNamespacePSA(ctx)
	if err != nil {
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("could not read pool namespace PSA label: %v", err)
		res.Remediation = psaRemediation(probe.Namespace())
		return res
	}
	if !privileged {
		got := label
		if got == "" {
			got = "(unset)"
		}
		res.Status = CheckFail
		res.Detail = fmt.Sprintf("pool namespace %s enforce label is %s, not privileged", probe.Namespace(), got)
		res.Remediation = psaRemediation(probe.Namespace())
		return res
	}
	res.Status = CheckPass
	res.Detail = fmt.Sprintf("pool namespace %s is PodSecurity privileged", probe.Namespace())
	return res
}

func psaRemediation(ns string) string {
	return fmt.Sprintf("forkd, the husk pods, and the device plugin are privileged with hostPath mounts, so namespace %s MUST carry pod-security.kubernetes.io/enforce=privileged. Label it: kubectl label namespace %s pod-security.kubernetes.io/enforce=privileged --overwrite. Create the namespace first if it does not exist.", ns, ns)
}

// DoctorExitCode is 1 if any check failed, 0 otherwise. Warns do not affect the
// exit code so an operator can run the preflight in a gate and only block on
// hard failures.
func DoctorExitCode(results []CheckResult) int {
	for _, r := range results {
		if r.Status == CheckFail {
			return 1
		}
	}
	return 0
}

// FormatDoctorReport renders the results as a human- and LLM-legible report:
// one line per check with a PASS/WARN/FAIL marker and detail, and an indented
// remediation line under each warn/fail. A trailing summary states the counts.
// Passing checks carry no remediation line. The report contains presence and
// counts only, never a secret value.
func FormatDoctorReport(results []CheckResult) string {
	var b strings.Builder
	b.WriteString("mitos doctor: preflight checks\n\n")
	var pass, warn, fail int
	for _, r := range results {
		switch r.Status {
		case CheckPass:
			pass++
		case CheckWarn:
			warn++
		case CheckFail:
			fail++
		}
		fmt.Fprintf(&b, "  [%s] %s: %s\n", marker(r.Status), r.Name, r.Detail)
		if r.Status != CheckPass && r.Remediation != "" {
			fmt.Fprintf(&b, "         remediation: %s\n", r.Remediation)
		}
	}
	fmt.Fprintf(&b, "\n%d passed, %d warnings, %d failed\n", pass, warn, fail)
	if fail > 0 {
		b.WriteString("preflight FAILED; fix the failing checks above before deploying.\n")
	} else if warn > 0 {
		b.WriteString("preflight OK (with warnings).\n")
	} else {
		b.WriteString("preflight OK.\n")
	}
	return b.String()
}

func marker(s CheckStatus) string {
	switch s {
	case CheckPass:
		return "PASS"
	case CheckWarn:
		return "WARN"
	case CheckFail:
		return "FAIL"
	default:
		return strings.ToUpper(string(s))
	}
}

// runDoctor runs the checks, prints the report to out, and returns the exit
// code. It is the testable core of cmdDoctor: cmd/mitos builds the real probe
// and calls the exported wiring; tests pass a fake probe directly.
func runDoctor(ctx context.Context, probe DoctorProbe, out, _ io.Writer) int {
	results := RunDoctorChecks(ctx, probe)
	fmt.Fprint(out, FormatDoctorReport(results))
	return DoctorExitCode(results)
}
