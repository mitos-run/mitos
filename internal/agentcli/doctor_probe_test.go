package agentcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleListed(t *testing.T) {
	const procModules = "nf_tables 12345 0 - Live 0xffff\n" +
		"vhost_vsock 6789 0 - Live 0xffff\n" +
		"tun 4444 2 - Live 0xffff\n"
	for _, name := range []string{"nf_tables", "vhost_vsock", "tun"} {
		if !moduleListed(strings.NewReader(procModules), name) {
			t.Errorf("moduleListed(%q) = false, want true", name)
		}
	}
	if moduleListed(strings.NewReader(procModules), "overlay") {
		t.Error("moduleListed(overlay) = true, want false")
	}
}

func TestRealProbeKVMDeviceSeam(t *testing.T) {
	// Point the KVM seam at a path that does not exist: a present-but-missing
	// device must report absent without erroring, so the check can fail cleanly.
	p := &realProbe{kvmPath: filepath.Join(t.TempDir(), "no-kvm")}
	present, err := p.KVMDevice(context.Background())
	if err != nil {
		t.Fatalf("KVMDevice err = %v, want nil for a missing path", err)
	}
	if present {
		t.Error("KVMDevice = true for a missing path, want false")
	}
}

func TestRealProbeKernelModuleSeam(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "modules")
	if err := os.WriteFile(modPath, []byte("tun 1 0 - Live 0x0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &realProbe{modulesPath: modPath}
	loaded, err := p.KernelModuleLoaded(context.Background(), "tun")
	if err != nil || !loaded {
		t.Fatalf("KernelModuleLoaded(tun) = %v, %v; want true, nil", loaded, err)
	}
	loaded, err = p.KernelModuleLoaded(context.Background(), "nf_tables")
	if err != nil || loaded {
		t.Fatalf("KernelModuleLoaded(nf_tables) = %v, %v; want false, nil", loaded, err)
	}
}

func TestRealProbeGuestKernelSeam(t *testing.T) {
	dir := t.TempDir()
	kpath := filepath.Join(dir, "vmlinux")
	// Missing file: absent, no error, path echoed for the remediation.
	p := &realProbe{kernelPath: kpath}
	present, path, err := p.GuestKernelStaged(context.Background())
	if err != nil || present {
		t.Fatalf("missing kernel = %v, %v; want false, nil", present, err)
	}
	if path != kpath {
		t.Errorf("path = %q, want %q", path, kpath)
	}
	// Empty file: not staged.
	if err := os.WriteFile(kpath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if present, _, _ := p.GuestKernelStaged(context.Background()); present {
		t.Error("empty kernel file reported as staged, want not staged")
	}
	// Non-empty file: staged.
	if err := os.WriteFile(kpath, []byte("ELF..."), 0o644); err != nil {
		t.Fatal(err)
	}
	if present, _, _ := p.GuestKernelStaged(context.Background()); !present {
		t.Error("non-empty kernel file reported as not staged, want staged")
	}
}

func TestRealProbeClusterChecksErrorWithoutClient(t *testing.T) {
	// With no client, the cluster checks must return a probe error (not panic),
	// which RunDoctorChecks folds into failing/warning checks so the node verdict
	// still prints.
	p := NewRealProbe(DoctorProbeConfig{Namespace: "mitos"})
	if _, err := p.PKISecretPresent(context.Background(), "mitos-ca"); err == nil {
		t.Error("PKISecretPresent with no client = nil err, want an error")
	}
	if _, _, err := p.PoolNamespacePSA(context.Background()); err == nil {
		t.Error("PoolNamespacePSA with no client = nil err, want an error")
	}
	// The PKI check folds the probe error into a CheckFail; the pull-secret check
	// folds it into a CheckWarn (non-blocking).
	results := RunDoctorChecks(context.Background(), p)
	if c := findCheck(t, results, "pki-secrets"); c.Status != CheckFail {
		t.Errorf("pki-secrets with no client = %s, want fail", c.Status)
	}
	if c := findCheck(t, results, "image-pull-secret"); c.Status != CheckWarn {
		t.Errorf("image-pull-secret with no client = %s, want warn", c.Status)
	}
}

func TestNewRealProbeDefaults(t *testing.T) {
	p := NewRealProbe(DoctorProbeConfig{}).(*realProbe)
	if p.namespace != "mitos" {
		t.Errorf("default namespace = %q, want mitos", p.namespace)
	}
	if p.kernelPath != defaultKernelPath {
		t.Errorf("default kernel path = %q, want %q", p.kernelPath, defaultKernelPath)
	}
}
