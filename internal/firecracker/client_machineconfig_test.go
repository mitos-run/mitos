package firecracker

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSetMachineConfigHugePages2M proves the engine can request 2 MiB
// hugepage-backed guest memory (issue #167): with hugePages "2M" the
// PUT /machine-config body carries huge_pages":"2M" so Firecracker backs the
// guest memory with hugetlbfs and each restore fault moves 2 MiB, not 4 KiB.
func TestSetMachineConfigHugePages2M(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.SetMachineConfig(1, 256, "2M"); err != nil {
		t.Fatalf("SetMachineConfig: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/machine-config")
	var got MachineConfig
	if err := json.Unmarshal(req.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := MachineConfig{VcpuCount: 1, MemSizeMib: 256, HugePages: "2M"}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

// TestSetMachineConfigOmitsHugePagesWhenDefault proves the default (4 KiB base
// pages) path is byte-identical to the pre-field behavior: with no huge pages
// requested, huge_pages is omitted entirely from the request body, so a node or
// snapshot that never asked for hugepages behaves exactly as before.
func TestSetMachineConfigOmitsHugePagesWhenDefault(t *testing.T) {
	srv, c := startFakeFCServer(t)
	if err := c.SetMachineConfig(2, 512, ""); err != nil {
		t.Fatalf("SetMachineConfig: %v", err)
	}
	req := srv.find(t, http.MethodPut, "/machine-config")
	if got := string(req.body); contains(got, "huge_pages") {
		t.Errorf("expected no huge_pages key in default body, got %s", got)
	}
}

// TestSetMachineConfigRejectsUnknownHugePages proves the value is validated with
// an actionable, LLM-legible error (issue #28) rather than forwarding a value
// Firecracker would reject with an opaque 400. Only "" (default) and "2M" are
// supported page granularities.
func TestSetMachineConfigRejectsUnknownHugePages(t *testing.T) {
	_, c := startFakeFCServer(t)
	err := c.SetMachineConfig(1, 256, "1G")
	if err == nil {
		t.Fatal("expected error for unsupported huge_pages value, got nil")
	}
	if !contains(err.Error(), "2M") || !contains(err.Error(), "1G") {
		t.Errorf("error should name the bad value and the valid option, got: %v", err)
	}
}
