package agentcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// fakeProbe is an in-memory DoctorProbe whose answers are set per field, so the
// pure check functions can be exercised on darwin without touching /dev, /proc,
// /sys, or a real cluster. A zero fakeProbe answers "nothing present", which
// drives every check to fail; tests flip individual fields to pass/warn cases.
type fakeProbe struct {
	kvmPresent    bool
	kvmErr        error
	loadedModules map[string]bool
	kernelStaged  bool
	kernelPath    string
	pkiSecrets    map[string]bool // name -> present
	pullSecret    bool
	psaPrivileged bool
	psaLabelValue string
	namespace     string
}

func (f *fakeProbe) KVMDevice(context.Context) (bool, error) { return f.kvmPresent, f.kvmErr }

func (f *fakeProbe) KernelModuleLoaded(_ context.Context, name string) (bool, error) {
	return f.loadedModules[name], nil
}

func (f *fakeProbe) GuestKernelStaged(context.Context) (present bool, path string, err error) {
	return f.kernelStaged, f.kernelPath, nil
}

func (f *fakeProbe) PKISecretPresent(_ context.Context, name string) (bool, error) {
	return f.pkiSecrets[name], nil
}

func (f *fakeProbe) ImagePullSecretPresent(context.Context) (bool, error) { return f.pullSecret, nil }

func (f *fakeProbe) PoolNamespacePSA(context.Context) (privileged bool, label string, err error) {
	return f.psaPrivileged, f.psaLabelValue, nil
}

func (f *fakeProbe) Namespace() string { return f.namespace }

// healthyProbe returns a fakeProbe where every check passes.
func healthyProbe() *fakeProbe {
	return &fakeProbe{
		kvmPresent: true,
		loadedModules: map[string]bool{
			"nf_tables":   true,
			"vhost_vsock": true,
			"tun":         true,
		},
		kernelStaged: true,
		kernelPath:   "/var/lib/mitos/vmlinux",
		pkiSecrets: map[string]bool{
			"mitos-ca":             true,
			"mitos-forkd-tls":      true,
			"mitos-controller-tls": true,
		},
		pullSecret:    true,
		psaPrivileged: true,
		psaLabelValue: "privileged",
		namespace:     "mitos",
	}
}

func TestDoctorHealthyAllPass(t *testing.T) {
	results := RunDoctorChecks(context.Background(), healthyProbe())
	if len(results) == 0 {
		t.Fatal("expected checks to run")
	}
	for _, r := range results {
		if r.Status != CheckPass {
			t.Errorf("check %q = %s (%s), want pass", r.Name, r.Status, r.Detail)
		}
	}
}

func TestDoctorKVMMissingFails(t *testing.T) {
	p := healthyProbe()
	p.kvmPresent = false
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "kvm-device")
	if r.Status != CheckFail {
		t.Fatalf("kvm-device status = %s, want fail", r.Status)
	}
	if r.Remediation == "" {
		t.Fatal("a failing check must carry a remediation")
	}
	if !strings.Contains(r.Remediation, "/dev/kvm") {
		t.Errorf("kvm remediation = %q, want it to name /dev/kvm", r.Remediation)
	}
}

func TestDoctorMissingModuleFails(t *testing.T) {
	p := healthyProbe()
	p.loadedModules["nf_tables"] = false
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "kernel-module-nf_tables")
	if r.Status != CheckFail {
		t.Fatalf("nf_tables status = %s, want fail", r.Status)
	}
	// The nf_tables remediation must name BOTH failure modes from issue #174.
	if !strings.Contains(r.Remediation, "egress") || !strings.Contains(r.Remediation, "kube-proxy") {
		t.Errorf("nf_tables remediation = %q, want it to name husk egress + kube-proxy", r.Remediation)
	}
}

func TestDoctorKernelNotStagedFails(t *testing.T) {
	p := healthyProbe()
	p.kernelStaged = false
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "guest-kernel")
	if r.Status != CheckFail {
		t.Fatalf("guest-kernel status = %s, want fail", r.Status)
	}
	if !strings.Contains(r.Remediation, "kernel-provisioner") {
		t.Errorf("guest-kernel remediation = %q, want it to name the kernel-provisioner", r.Remediation)
	}
	if !strings.Contains(r.Remediation, p.kernelPath) {
		t.Errorf("guest-kernel remediation = %q, want it to name the path %q", r.Remediation, p.kernelPath)
	}
}

func TestDoctorMissingPKIFails(t *testing.T) {
	p := healthyProbe()
	p.pkiSecrets["mitos-ca"] = false
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "pki-secrets")
	if r.Status != CheckFail {
		t.Fatalf("pki-secrets status = %s, want fail", r.Status)
	}
	if !strings.Contains(r.Detail, "mitos-ca") {
		t.Errorf("pki detail = %q, want it to name the missing secret", r.Detail)
	}
}

func TestDoctorMissingPullSecretWarns(t *testing.T) {
	p := healthyProbe()
	p.pullSecret = false
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "image-pull-secret")
	// A missing pull secret is a WARN: public images still pull, only private
	// registries need it. It must not flip the run to fail on its own.
	if r.Status != CheckWarn {
		t.Fatalf("image-pull-secret status = %s, want warn", r.Status)
	}
}

func TestDoctorMissingPSALabelFails(t *testing.T) {
	p := healthyProbe()
	p.psaPrivileged = false
	p.psaLabelValue = "restricted"
	r := findCheck(t, RunDoctorChecks(context.Background(), p), "psa-privileged")
	if r.Status != CheckFail {
		t.Fatalf("psa-privileged status = %s, want fail", r.Status)
	}
	if !strings.Contains(r.Remediation, "privileged") {
		t.Errorf("psa remediation = %q, want it to name the privileged label", r.Remediation)
	}
}

func TestDoctorNeverLeaksSecretValues(t *testing.T) {
	// Secret VALUES are never read by the probe interface (it answers presence
	// booleans only), so the report cannot leak one. Guard the report formatting:
	// it reports presence/counts, never a value. This is a structural assertion
	// that the probe surface exposes booleans, not values, for the secret checks.
	results := RunDoctorChecks(context.Background(), healthyProbe())
	report := FormatDoctorReport(results)
	for _, forbidden := range []string{"BEGIN CERTIFICATE", "PRIVATE KEY", "token="} {
		if strings.Contains(report, forbidden) {
			t.Errorf("report leaked a secret-shaped string %q", forbidden)
		}
	}
}

func TestDoctorExitCodeFailWhenAnyFail(t *testing.T) {
	p := healthyProbe()
	p.kvmPresent = false
	results := RunDoctorChecks(context.Background(), p)
	if code := DoctorExitCode(results); code == 0 {
		t.Fatal("exit code = 0 with a failing check, want non-zero")
	}
}

func TestDoctorExitCodeZeroOnPassAndWarn(t *testing.T) {
	p := healthyProbe()
	p.pullSecret = false // warn only
	results := RunDoctorChecks(context.Background(), p)
	if code := DoctorExitCode(results); code != 0 {
		t.Fatalf("exit code = %d with only a warn, want 0", code)
	}
}

func TestDoctorReportLabelsStatusAndRemediation(t *testing.T) {
	p := healthyProbe()
	p.kvmPresent = false
	report := FormatDoctorReport(RunDoctorChecks(context.Background(), p))
	if !strings.Contains(report, "FAIL") {
		t.Errorf("report = %q, want a FAIL marker", report)
	}
	if !strings.Contains(report, "remediation:") {
		t.Errorf("report = %q, want a remediation line for the failing check", report)
	}
	// A passing check should not print a remediation line.
	healthyReport := FormatDoctorReport(RunDoctorChecks(context.Background(), healthyProbe()))
	if strings.Contains(healthyReport, "remediation:") {
		t.Errorf("healthy report should carry no remediation lines, got %q", healthyReport)
	}
}

func TestDoctorCommandExitCodeAndOutput(t *testing.T) {
	// The cmdDoctor wiring: a probe that fails one check exits non-zero and
	// prints the report; a healthy probe exits 0.
	var out, errw bytes.Buffer
	bad := healthyProbe()
	bad.kvmPresent = false
	code := runDoctor(context.Background(), bad, &out, &errw)
	if code == 0 {
		t.Fatalf("runDoctor exit = 0 with a failing check, want non-zero")
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("runDoctor stdout = %q, want a FAIL marker", out.String())
	}

	var out2, errw2 bytes.Buffer
	if code := runDoctor(context.Background(), healthyProbe(), &out2, &errw2); code != 0 {
		t.Fatalf("runDoctor exit = %d on a healthy probe, want 0", code)
	}
}

func findCheck(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no check named %q in results", name)
	return CheckResult{}
}
