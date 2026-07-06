package husk

import (
	"bytes"
	"reflect"
	"testing"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
)

func TestActivateRequestCarriesEgressConfig(t *testing.T) {
	in := ActivateRequest{
		SnapshotDir: "/snap",
		Egress:      "deny",
		Allow:       []string{"api.example.com:443", "10.0.0.5:5432"},
		Network:     &vsock.NotifyForkedNetwork{GuestIP: "10.200.0.2", GatewayIP: "10.200.0.1", PrefixLen: 30, ResolverIP: "169.254.1.1"},
	}
	var buf bytes.Buffer
	if err := WriteRequest(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Egress != "deny" || len(out.Allow) != 2 || out.Allow[0] != "api.example.com:443" {
		t.Errorf("egress config did not round-trip: %+v", out)
	}
	if out.Network == nil || out.Network.ResolverIP != "169.254.1.1" {
		t.Errorf("network did not round-trip: %+v", out.Network)
	}
}

func TestActivateRequestRoundTrip(t *testing.T) {
	want := ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		NetworkOverrides: []firecracker.NetworkOverride{
			{IfaceID: "eth0", HostDevName: "tap-fork-1"},
		},
		Env:     map[string]string{"PATH": "/usr/bin", "LANG": "C"},
		Secrets: map[string]string{"API_KEY": "s3cr3t-value"},
		Network: &vsock.NotifyForkedNetwork{
			GuestIP:   "10.0.0.2",
			GatewayIP: "10.0.0.1",
			PrefixLen: 30,
		},
		Volumes: []vsock.VolumeMountEntry{
			{Device: "/dev/vdb", MountPath: "/data"},
		},
	}

	var buf bytes.Buffer
	if err := WriteRequest(&buf, want); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("WriteRequest did not newline-terminate: %q", buf.String())
	}

	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestActivateResultRoundTrip(t *testing.T) {
	want := ActivateResult{
		OK:        true,
		VsockPath: "/run/husk/vsock.sock",
		LatencyMs: 4.275,
	}

	var buf bytes.Buffer
	if err := WriteResult(&buf, want); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	got, err := ReadResult(&buf)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestActivateResultErrorRoundTrip(t *testing.T) {
	want := ActivateResult{OK: false, Error: "load snapshot: boom"}

	var buf bytes.Buffer
	if err := WriteResult(&buf, want); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	got, err := ReadResult(&buf)
	if err != nil {
		t.Fatalf("ReadResult: %v", err)
	}
	if got.OK || got.Error != want.Error {
		t.Fatalf("error result mismatch: got %+v want %+v", got, want)
	}
}

func TestForkSnapshotRequestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: "/var/lib/mitos/forks/fork-1", PauseSource: true}
	if err := WriteForkSnapshotRequest(&buf, want); err != nil {
		t.Fatalf("WriteForkSnapshotRequest: %v", err)
	}
	got, err := ReadForkSnapshotRequest(&buf)
	if err != nil {
		t.Fatalf("ReadForkSnapshotRequest: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestForkSnapshotResultRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := ForkSnapshotResult{OK: true, SnapshotDir: "/var/lib/mitos/forks/fork-1", LatencyMs: 12.5}
	if err := WriteForkSnapshotResult(&buf, want); err != nil {
		t.Fatalf("WriteForkSnapshotResult: %v", err)
	}
	got, err := ReadForkSnapshotResult(&buf)
	if err != nil {
		t.Fatalf("ReadForkSnapshotResult: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestSpawnVMRequestRoundTrip(t *testing.T) {
	want := SpawnVMRequest{
		VMID: "fork-7",
		Activate: ActivateRequest{
			SnapshotDir:    "/var/lib/mitos/forks/fork-7",
			ExpectedDigest: "sha256:abc",
			Egress:         "deny",
			Allow:          []string{"api.example.com:443"},
			Env:            map[string]string{"LANG": "C"},
			Secrets:        map[string]string{"API_KEY": "s3cr3t-value"},
			Token:          "bearer-token",
			Network: &vsock.NotifyForkedNetwork{
				GuestIP:   "10.0.0.2",
				GatewayIP: "10.0.0.1",
				PrefixLen: 30,
			},
			NetworkOverrides: []firecracker.NetworkOverride{
				{IfaceID: "eth0", HostDevName: "tap-fork-7"},
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteSpawnVMRequest(&buf, want); err != nil {
		t.Fatalf("WriteSpawnVMRequest: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("WriteSpawnVMRequest did not newline-terminate: %q", buf.String())
	}
	got, err := ReadSpawnVMRequest(&buf)
	if err != nil {
		t.Fatalf("ReadSpawnVMRequest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestSpawnVMResultRoundTrip(t *testing.T) {
	want := SpawnVMResult{
		OK:        true,
		VMID:      "fork-7",
		VsockPath: "/run/husk/fork-7/vsock.sock",
		LatencyMs: 6.25,
	}
	var buf bytes.Buffer
	if err := WriteSpawnVMResult(&buf, want); err != nil {
		t.Fatalf("WriteSpawnVMResult: %v", err)
	}
	got, err := ReadSpawnVMResult(&buf)
	if err != nil {
		t.Fatalf("ReadSpawnVMResult: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestSpawnVMResultErrorRoundTrip(t *testing.T) {
	want := SpawnVMResult{OK: false, VMID: "fork-7", Error: "spawn-vm prepare vm \"fork-7\": boom"}
	var buf bytes.Buffer
	if err := WriteSpawnVMResult(&buf, want); err != nil {
		t.Fatalf("WriteSpawnVMResult: %v", err)
	}
	got, err := ReadSpawnVMResult(&buf)
	if err != nil {
		t.Fatalf("ReadSpawnVMResult: %v", err)
	}
	if got.OK || got.Error != want.Error {
		t.Fatalf("error result mismatch: got %+v want %+v", got, want)
	}
}
