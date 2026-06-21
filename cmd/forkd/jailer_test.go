package main

import (
	"errors"
	"strings"
	"testing"
)

func sameFSAlways(string, string) (bool, error) { return true, nil }

func TestParseUIDRange(t *testing.T) {
	cases := []struct {
		in      string
		lo, hi  uint32
		wantErr bool
	}{
		{in: "64000-64999", lo: 64000, hi: 64999},
		{in: "100-100", lo: 100, hi: 100},
		{in: "abc", wantErr: true},
		{in: "64000", wantErr: true},
		{in: "64000-", wantErr: true},
		{in: "-64999", wantErr: true},
		{in: "64999-64000", wantErr: true}, // low above high
		{in: "0-100", wantErr: true},       // uid 0 is root; never jail as root
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		lo, hi, err := parseUIDRange(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseUIDRange(%q) accepted, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUIDRange(%q): %v", c.in, err)
			continue
		}
		if lo != c.lo || hi != c.hi {
			t.Errorf("parseUIDRange(%q) = %d-%d, want %d-%d", c.in, lo, hi, c.lo, c.hi)
		}
	}
}

func TestBuildJailerConfigDisabled(t *testing.T) {
	cfg, err := buildJailerConfig("", "/srv/jailer", "64000-64999", t.TempDir(), 1000, sameFSAlways)
	if err != nil {
		t.Fatalf("empty --jailer must disable the jailer, got error: %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("empty --jailer produced an enabled config")
	}
}

func TestBuildJailerConfigRequiresRoot(t *testing.T) {
	dir := t.TempDir()
	_, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "64000-64999", dir+"/data", 1000, sameFSAlways)
	if err == nil {
		t.Fatal("nonroot forkd with --jailer must fail closed")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Fatalf("error should name the root requirement: %v", err)
	}
}

func TestBuildJailerConfigRefusesCrossFilesystem(t *testing.T) {
	dir := t.TempDir()
	crossFS := func(string, string) (bool, error) { return false, nil }
	_, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "64000-64999", dir+"/data", 0, crossFS)
	if err == nil {
		t.Fatal("chroot base on a different filesystem from the data dir must fail closed")
	}
	if !strings.Contains(err.Error(), "filesystem") {
		t.Fatalf("error should explain the same-filesystem requirement: %v", err)
	}
}

func TestBuildJailerConfigPropagatesFSCheckError(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("stat exploded")
	failFS := func(string, string) (bool, error) { return false, boom }
	_, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "64000-64999", dir+"/data", 0, failFS)
	if !errors.Is(err, boom) {
		t.Fatalf("expected fs check error to propagate, got %v", err)
	}
}

func TestBuildJailerConfigValid(t *testing.T) {
	dir := t.TempDir()
	cfg, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "64000-64999", dir+"/data", 0, sameFSAlways)
	if err != nil {
		t.Fatalf("buildJailerConfig: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("config should be enabled")
	}
	if cfg.ChrootBaseDir != dir+"/jail" {
		t.Fatalf("ChrootBaseDir = %q", cfg.ChrootBaseDir)
	}
	if cfg.UIDRange != [2]uint32{64000, 64999} {
		t.Fatalf("UIDRange = %v", cfg.UIDRange)
	}
}

func TestBuildJailerConfigBadRangeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "0-10", dir+"/data", 0, sameFSAlways); err == nil {
		t.Fatal("uid range including 0 must fail closed")
	}
}

// TestJailerRequiredCapabilities pins the EXACT capability set forkd must retain
// to build each VM's jail, as a single tested source of truth. The kernel
// ENFORCING the drop of everything else is KVM/non-root-gated (the CI runner is
// root, see fork-correctness section "CI job"); this test only asserts the cap
// LIST is correctly computed, which is darwin-testable. The list here is the
// authority the DaemonSet securityContext.capabilities.add and the buildJailerConfig
// error message must agree with.
func TestJailerRequiredCapabilities(t *testing.T) {
	want := []string{
		"CAP_SYS_ADMIN", // cgroup and namespace setup
		"CAP_CHOWN",     // hand the chroot to the per-VM uid
		"CAP_SETUID",    // drop to the per-VM uid
		"CAP_SETGID",    // drop to the per-VM gid
		"CAP_MKNOD",     // create /dev/kvm and /dev/net/tun nodes in the chroot
	}
	got := jailerRequiredCapabilities()
	if len(got) != len(want) {
		t.Fatalf("jailerRequiredCapabilities() = %v (%d caps), want %v (%d caps)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capability[%d] = %q, want %q (order is part of the contract so the list is diff-stable)", i, got[i], want[i])
		}
	}
}

// TestJailerRequiredCapabilitiesNoExtras asserts the set is MINIMAL: forkd must
// not silently start claiming a broad capability (the threat-model residual is
// the builder cap set, so a regression that added CAP_SYS_RAWIO or CAP_NET_ADMIN
// must be caught here). The allowlist below is the only set the function may
// return; anything outside it fails.
func TestJailerRequiredCapabilitiesNoExtras(t *testing.T) {
	allowed := map[string]bool{
		"CAP_SYS_ADMIN": true,
		"CAP_CHOWN":     true,
		"CAP_SETUID":    true,
		"CAP_SETGID":    true,
		"CAP_MKNOD":     true,
	}
	seen := map[string]bool{}
	for _, c := range jailerRequiredCapabilities() {
		if !allowed[c] {
			t.Fatalf("jailerRequiredCapabilities() returned unexpected capability %q; widening the builder cap set is a threat-model change and must be a reviewed diff", c)
		}
		if seen[c] {
			t.Fatalf("jailerRequiredCapabilities() returned duplicate capability %q", c)
		}
		seen[c] = true
	}
}

// TestBuildJailerConfigErrorNamesEveryRequiredCapability asserts the nonroot
// failure message names EVERY capability the builder needs, so the operator sees
// the full set to grant. This ties the human-facing error to the tested list.
func TestBuildJailerConfigErrorNamesEveryRequiredCapability(t *testing.T) {
	dir := t.TempDir()
	_, err := buildJailerConfig("/usr/local/bin/jailer", dir+"/jail", "64000-64999", dir+"/data", 1000, sameFSAlways)
	if err == nil {
		t.Fatal("nonroot forkd with --jailer must fail closed")
	}
	for _, cap := range jailerRequiredCapabilities() {
		if !strings.Contains(err.Error(), cap) {
			t.Fatalf("nonroot error does not name required capability %q: %v", cap, err)
		}
	}
}
