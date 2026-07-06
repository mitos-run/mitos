package husk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/usage"
	"mitos.run/mitos/internal/vsock"
)

// newTestStubWithID is newTestStub but lets a test pin the pod's vm-id so it can
// assert the single metering Sample carries it (the id the controller maps to an
// org). The other seams are the same no-op fakes newTestStub uses.
func newTestStubWithID(t *testing.T, id string, vm *fakeVMM, ready guestReady) *Stub {
	t.Helper()
	return New(firecracker.VMConfig{ID: id}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return vm, nil },
		Ready:  ready,
		Notify: (&fakeNotifier{}).notify,
		Verify: verifyOK,
	})
}

// TestMeteringEmptyBeforeActivate asserts a husk pod meters NOTHING before a
// successful activate (StateNew and StateDormant), so a scrape during the warm
// window is a clean empty report, never a 5xx.
func TestMeteringEmptyBeforeActivate(t *testing.T) {
	s := newTestStub(t, &fakeVMM{}, readyOK)

	// StateNew (before Prepare).
	if rep := s.Metering(); len(rep.Sandboxes) != 0 {
		t.Fatalf("StateNew metering must be empty, got %d sandboxes", len(rep.Sandboxes))
	}

	// StateDormant (after Prepare).
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	rep := s.Metering()
	if len(rep.Sandboxes) != 0 {
		t.Fatalf("StateDormant metering must be empty, got %d sandboxes", len(rep.Sandboxes))
	}
	if len(rep.Templates) != 0 {
		t.Fatalf("StateDormant metering must have no templates, got %d", len(rep.Templates))
	}
}

// TestMeteringSingleVMAfterActivate asserts an activated husk pod reports EXACTLY
// one sample and that its id is the pod's vm-id (the id the controller attributes
// to an org).
func TestMeteringSingleVMAfterActivate(t *testing.T) {
	const vmID = "husk-pod-acme-xyz"
	s := newTestStubWithID(t, vmID, &fakeVMM{}, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}

	rep := s.Metering()
	if len(rep.Sandboxes) != 1 {
		t.Fatalf("active metering must have exactly one sample, got %d", len(rep.Sandboxes))
	}
	if rep.Sandboxes[0].ID != vmID {
		t.Fatalf("sample id = %q, want the pod vm-id %q", rep.Sandboxes[0].ID, vmID)
	}
}

// newMeteredTestStub builds an activated-ready stub with every metering seam
// wired to a deterministic fake: the VMM reports firecracker pid meteredPID,
// MemStat returns (96 MiB unique, 416 MiB shared) for exactly that pid, the
// rootfs template seed is meteredSeedSize bytes, the per-activation clone
// diverges by meteredCloneExtra bytes, and the egress reader returns a
// cumulative per-tap byte counter that grows by meteredEgressStep per read.
func newMeteredTestStub(t *testing.T, id string) (*Stub, *atomic.Int64) {
	t.Helper()
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "rootfs.ext4")
	writeFileOfSize(t, seedPath, meteredSeedSize)

	var egressReads atomic.Int64
	s := New(firecracker.VMConfig{ID: id}, Options{
		Start:  func(cfg firecracker.VMConfig) (vmm, error) { return &fakeVMM{pid: meteredPID}, nil },
		Ready:  readyOK,
		Notify: (&fakeNotifier{}).notify,
		Verify: verifyOK,

		RootfsTemplatePath: seedPath,
		RootfsCoWDir:       filepath.Join(dir, "cow"),
		Reflink: func(src, dst string) error {
			// The fake clone diverges from the seed by meteredCloneExtra bytes so
			// the DiskUnique approximation (clone minus seed) is observable.
			writeFileOfSize(t, dst, meteredSeedSize+meteredCloneExtra)
			return nil
		},

		MemStat: func(pid int) (unique, shared int64) {
			if pid == meteredPID {
				return meteredMemUnique, meteredMemShared
			}
			return 0, 0
		},
		EgressBytes: func(tap string) int64 {
			if tap == "" {
				return 0
			}
			return egressReads.Add(1) * meteredEgressStep
		},
	})
	s.netRunner = (&recordingRunner{}).run
	return s, &egressReads
}

const (
	meteredPID        = 4242
	meteredMemUnique  = int64(96 << 20)  // 96 MiB private (unique) pages.
	meteredMemShared  = int64(416 << 20) // 416 MiB shared template pages.
	meteredSeedSize   = int64(4 << 20)   // 4 MiB rootfs template seed.
	meteredCloneExtra = int64(1 << 20)   // 1 MiB per-activation divergence.
	meteredEgressStep = int64(1000)      // cumulative egress bytes per read.
)

// writeFileOfSize creates path with exactly size apparent bytes (sparse).
func writeFileOfSize(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// activateMetered prepares and activates a metered stub with networking so the
// egress tap exists.
func activateMetered(t *testing.T, s *Stub) {
	t.Helper()
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir: "/snap",
		Egress:      "deny",
		Network: &vsock.NotifyForkedNetwork{
			GuestIP: "10.200.0.2", GatewayIP: "10.200.0.1", PrefixLen: 30,
		},
	})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}
}

// TestMeteringReportsMemoryDiskAndEgress is the regression test for the
// production all-zero MemGiBSeconds/StorageGiBHours bug: the activated husk
// pod's single metering sample must carry the VM's CoW-aware memory split
// (smaps_rollup unique+shared of the firecracker pid), the rootfs storage
// footprint (template seed shared, clone divergence unique), and the per-tap
// egress counter, not just the bare vm-id.
func TestMeteringReportsMemoryDiskAndEgress(t *testing.T) {
	const vmID = "husk-pod-metered"
	s, _ := newMeteredTestStub(t, vmID)
	activateMetered(t, s)

	rep := s.Metering()
	if len(rep.Sandboxes) != 1 {
		t.Fatalf("active metering must have exactly one sample, got %d", len(rep.Sandboxes))
	}
	sb := rep.Sandboxes[0]
	if sb.ID != vmID {
		t.Fatalf("sample id = %q, want %q", sb.ID, vmID)
	}
	if sb.MemoryUnique != meteredMemUnique {
		t.Errorf("MemoryUnique = %d, want %d (private pages of the firecracker pid)", sb.MemoryUnique, meteredMemUnique)
	}
	if sb.MemoryShared != meteredMemShared {
		t.Errorf("MemoryShared = %d, want %d (shared template pages)", sb.MemoryShared, meteredMemShared)
	}
	if sb.DiskShared != meteredSeedSize {
		t.Errorf("DiskShared = %d, want %d (rootfs template seed)", sb.DiskShared, meteredSeedSize)
	}
	if sb.DiskUnique != meteredCloneExtra {
		t.Errorf("DiskUnique = %d, want %d (clone divergence over the seed)", sb.DiskUnique, meteredCloneExtra)
	}
	if sb.EgressBytes != meteredEgressStep {
		t.Errorf("EgressBytes = %d, want %d (per-tap nft counter)", sb.EgressBytes, meteredEgressStep)
	}
	// The single-VM report's CoW rollup: the one sample's shared set is counted
	// once, so the resident footprint is unique + shared.
	if want := meteredMemUnique + meteredMemShared; rep.UsedCoWAware != want {
		t.Errorf("UsedCoWAware = %d, want %d", rep.UsedCoWAware, want)
	}
}

// staticHuskLister serves a fixed husk pod list to the usage.HuskSource.
type staticHuskLister struct{ pods []usage.HuskPod }

func (l staticHuskLister) ListHuskPods(context.Context) ([]usage.HuskPod, error) {
	return l.pods, nil
}

// TestHuskMeteringBillsMemoryStorageAndEgress reproduces the production bug end
// to end: a realistic hosted pipeline (husk stub -> in-pod GET /v1/metering ->
// usage.HuskSource -> usage.Integrate) must yield a usage record whose
// MemGiBSeconds and StorageGiBHours integrate like VCPUSeconds does, and whose
// EgressBytes is the counter delta. Before the fix the stub reported a bare
// vm-id sample, so every hosted record had MemGiBSeconds=0 and
// StorageGiBHours=0 while VCPUSeconds metered.
func TestHuskMeteringBillsMemoryStorageAndEgress(t *testing.T) {
	const vmID = "husk-pod-pipeline"
	s, _ := newMeteredTestStub(t, vmID)
	activateMetered(t, s)

	// Serve the stub's live report exactly like cmd/husk-stub does on the in-pod
	// sandbox mux (unauthenticated GET /v1/metering).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Metering())
	}))
	defer srv.Close()

	lister := staticHuskLister{pods: []usage.HuskPod{{
		VMID:     vmID,
		APIID:    "sb-cafe1234",
		OrgID:    "org-acme",
		Endpoint: strings.TrimPrefix(srv.URL, "http://"),
	}}}

	// Three scrapes at a sub-window cadence (like the collector's), spanning one
	// full window: the rate levels integrate over [t0, t0+60) and the two
	// in-window scrapes give the counter units a same-window delta.
	t0 := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	scrapes := []time.Time{t0, t0.Add(30 * time.Second), t0.Add(60 * time.Second)}
	var call int
	src := usage.NewHuskSource(lister, nil, srv.Client(), "", func() time.Time {
		at := scrapes[call%len(scrapes)]
		call++
		return at
	})

	var samples []usage.Sample
	for range scrapes {
		got, err := src.Collect(context.Background())
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		samples = append(samples, got...)
	}

	recs := usage.Integrate(samples, usage.DefaultConfig())
	if len(recs) != 1 {
		t.Fatalf("Integrate returned %d records, want 1: %+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.OrgID != "org-acme" || rec.SandboxID != "sb-cafe1234" {
		t.Fatalf("record attribution = (%q, %q), want (org-acme, sb-cafe1234)", rec.OrgID, rec.SandboxID)
	}
	if rec.VCPUSeconds != 60 {
		t.Errorf("VCPUSeconds = %v, want 60", rec.VCPUSeconds)
	}

	const gib = float64(1 << 30)
	wantMem := float64(meteredMemUnique+meteredMemShared) / gib * 60
	if diff := rec.MemGiBSeconds - wantMem; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("MemGiBSeconds = %v, want %v (resident level x 60s); production bug was 0", rec.MemGiBSeconds, wantMem)
	}
	wantStorage := float64(meteredSeedSize+meteredCloneExtra) / gib * (60.0 / 3600.0)
	if diff := rec.StorageGiBHours - wantStorage; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("StorageGiBHours = %v, want %v (rootfs bytes x window); production bug was 0", rec.StorageGiBHours, wantStorage)
	}
	// The egress counter grew by one meteredEgressStep between the window's two
	// in-window scrapes; the record bills the delta, never the cumulative value.
	if rec.EgressBytes != meteredEgressStep {
		t.Errorf("EgressBytes = %d, want %d (counter delta within the window)", rec.EgressBytes, meteredEgressStep)
	}
}

// TestMeteringReportIsSecretFree asserts the metering report never carries a
// secret value: activating with an env value, a secret value, and a bearer token,
// then marshaling the report, must not leak any of them.
func TestMeteringReportIsSecretFree(t *testing.T) {
	const (
		vmID        = "husk-pod-secret"
		envValue    = "ENV-VALUE-SHOULD-NOT-LEAK"
		secretValue = "SECRET-VALUE-SHOULD-NOT-LEAK"
		tokenValue  = "TOKEN-SHOULD-NOT-LEAK"
	)
	s := newTestStubWithID(t, vmID, &fakeVMM{}, readyOK)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{
		SnapshotDir: "/snap",
		Env:         map[string]string{"MY_ENV": envValue},
		Secrets:     map[string]string{"MY_SECRET": secretValue},
		Token:       tokenValue,
	})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}

	body, err := json.Marshal(s.Metering())
	if err != nil {
		t.Fatalf("marshal metering report: %v", err)
	}
	for _, secret := range []string{envValue, secretValue, tokenValue} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("metering report body leaked a secret value: %s", body)
		}
	}
}
