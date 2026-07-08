package firecracker

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestCreateSnapshotVMStateOnlyOmitsMemFile proves the live-cow fork capture
// (issue #832) issues PUT /snapshot/create with the Mitos vmstate-only type and
// NO mem_file_path: the 364ms guest-RAM copy a Full snapshot writes is skipped
// because the child boots its guest RAM from the parent's shared memfd (m5). A
// mem_file_path on this request would make Firecracker copy the whole guest RAM,
// which is exactly the cost this path eliminates.
func TestCreateSnapshotVMStateOnlyOmitsMemFile(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.CreateSnapshotVMStateOnly("/snap/vmstate"); err != nil {
		t.Fatalf("CreateSnapshotVMStateOnly: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/create")
	if contains(string(req.body), "mem_file_path") {
		t.Errorf("vmstate-only snapshot must omit mem_file_path, got %s", req.body)
	}
	var got SnapshotCreate
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SnapshotType != snapshotTypeMitosVmstateOnly {
		t.Errorf("snapshot_type = %q, want %q", got.SnapshotType, snapshotTypeMitosVmstateOnly)
	}
	if got.SnapshotPath != "/snap/vmstate" {
		t.Errorf("snapshot_path = %q, want /snap/vmstate", got.SnapshotPath)
	}
	if got.MemFilePath != "" {
		t.Errorf("mem_file_path = %q, want empty (no guest-RAM copy)", got.MemFilePath)
	}
}

// TestCreateSnapshotFullStillWritesMemFile pins the FALLBACK: the Full snapshot
// used when live-cow is off carries mem_file_path and the Full type exactly as
// before, so a non-live-cow fork is byte-for-byte unchanged.
func TestCreateSnapshotFullStillWritesMemFile(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.CreateSnapshot("/snap/mem", "/snap/vmstate"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/create")
	var got SnapshotCreate
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SnapshotType != snapshotTypeFull {
		t.Errorf("snapshot_type = %q, want %q", got.SnapshotType, snapshotTypeFull)
	}
	if got.MemFilePath != "/snap/mem" {
		t.Errorf("mem_file_path = %q, want /snap/mem", got.MemFilePath)
	}
}
