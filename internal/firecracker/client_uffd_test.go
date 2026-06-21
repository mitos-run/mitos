package firecracker

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestLoadSnapshotUFFD proves the UFFD restore path sends mem_backend with
// backend_type "Uffd" pointing at the handler socket, and OMITS mem_file_path
// entirely (issue #167). Firecracker rejects a load that carries both a memory
// file path and a UFFD backend, and a hugetlbfs-backed snapshot can ONLY be
// restored via UFFD, so the file path must not be present on this path.
func TestLoadSnapshotUFFD(t *testing.T) {
	srv, c := startFakeFCServer(t)
	overrides := []NetworkOverride{{IfaceID: "eth0", HostDevName: "tapfork9"}}
	if err := c.LoadSnapshotUFFD("/vmstate", "/run/uffd.sock", overrides); err != nil {
		t.Fatalf("LoadSnapshotUFFD: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/load")
	if contains(string(req.body), "mem_file_path") {
		t.Errorf("UFFD load must omit mem_file_path, got %s", req.body)
	}
	var got SnapshotLoad
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MemBackend == nil || got.MemBackend.BackendType != "Uffd" || got.MemBackend.BackendPath != "/run/uffd.sock" {
		t.Errorf("mem_backend = %+v, want Uffd /run/uffd.sock", got.MemBackend)
	}
	if got.SnapshotPath != "/vmstate" || got.ResumeVM {
		t.Errorf("load fields wrong: %+v", got)
	}
	if len(got.NetworkOverrides) != 1 || got.NetworkOverrides[0] != overrides[0] {
		t.Errorf("network_overrides = %+v, want %+v", got.NetworkOverrides, overrides)
	}
}

// TestLoadSnapshotFileOmitsMemBackend proves the existing file-mapping restore is
// unchanged: it still sends mem_file_path and omits mem_backend, so non-hugepage
// snapshots restore exactly as before.
func TestLoadSnapshotFileOmitsMemBackend(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.LoadSnapshot("/mem", "/vmstate", true); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/snapshot/load")
	if contains(string(req.body), "mem_backend") {
		t.Errorf("file-backed load must omit mem_backend, got %s", req.body)
	}
	if !contains(string(req.body), "mem_file_path") {
		t.Errorf("file-backed load must carry mem_file_path, got %s", req.body)
	}
}
