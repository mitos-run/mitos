package husk

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/paperclipinc/sandbox/internal/firecracker"
)

func TestActivateRequestRoundTrip(t *testing.T) {
	want := ActivateRequest{
		SnapshotDir: "/data/templates/tmpl-a/snapshot",
		NetworkOverrides: []firecracker.NetworkOverride{
			{IfaceID: "eth0", HostDevName: "tap-fork-1"},
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
