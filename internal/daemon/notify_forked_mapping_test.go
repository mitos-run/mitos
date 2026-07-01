package daemon

// Unit tests for the vsock->proto NotifyForkedNetwork mapping introduced in
// issue #336: the proxy_endpoint and reset_upstreams fields must round-trip
// through toProtoNotifyForkedNetwork so the guest agent receives both values.

import (
	"testing"

	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"

	"mitos.run/mitos/internal/vsock"
)

// buildNotifyForkedRequest assembles an internalv1.NotifyForkedRequest from the
// same inputs as SandboxAPI.NotifyForked. It is a test helper that exercises
// toProtoNotifyForkedNetwork directly, without requiring a live gRPC connection.
func buildNotifyForkedRequest(generation uint64, entropy []byte, gn *vsock.NotifyForkedNetwork, volumes []vsock.VolumeMountEntry) *internalv1.NotifyForkedRequest {
	req := &internalv1.NotifyForkedRequest{
		Generation: generation,
		Entropy:    entropy,
		Network:    toProtoNotifyForkedNetwork(gn),
	}
	for _, v := range volumes {
		req.Volumes = append(req.Volumes, &internalv1.VolumeMountEntry{
			Device:    v.Device,
			MountPath: v.MountPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	return req
}

// TestNotifyForkedNetworkProxyFieldsMap verifies that ProxyEndpoint and
// ResetUpstreams on vsock.NotifyForkedNetwork are carried through to the proto
// message by toProtoNotifyForkedNetwork, so the guest agent receives both
// values on every fork.
func TestNotifyForkedNetworkProxyFieldsMap(t *testing.T) {
	gn := &vsock.NotifyForkedNetwork{
		GuestIP:        "10.0.0.6",
		GatewayIP:      "10.0.0.5",
		PrefixLen:      30,
		ProxyEndpoint:  "169.254.169.2:3128",
		ResetUpstreams: true,
	}
	req := buildNotifyForkedRequest(7, nil, gn, nil)
	if req.Network.ProxyEndpoint != "169.254.169.2:3128" || !req.Network.ResetUpstreams {
		t.Fatalf("proxy fields not mapped: %+v", req.Network)
	}
}

// TestNotifyForkedNetworkProxyFieldsNilNetwork verifies that
// toProtoNotifyForkedNetwork returns nil when the network config is nil, so
// callers that do not set network identity are unaffected.
func TestNotifyForkedNetworkProxyFieldsNilNetwork(t *testing.T) {
	req := buildNotifyForkedRequest(1, nil, nil, nil)
	if req.Network != nil {
		t.Fatalf("nil gn must produce nil network, got %+v", req.Network)
	}
}

// TestNotifyForkedNetworkProxyFieldsEmpty verifies that an empty ProxyEndpoint
// and false ResetUpstreams are passed through as zero values, leaving the
// guest's environment untouched when the proxy is disabled.
func TestNotifyForkedNetworkProxyFieldsEmpty(t *testing.T) {
	gn := &vsock.NotifyForkedNetwork{
		GuestIP:   "10.0.0.2",
		GatewayIP: "10.0.0.1",
		PrefixLen: 30,
	}
	req := buildNotifyForkedRequest(2, nil, gn, nil)
	if req.Network == nil {
		t.Fatal("non-nil gn must produce non-nil network")
	}
	if req.Network.ProxyEndpoint != "" || req.Network.ResetUpstreams {
		t.Fatalf("empty proxy fields must be zero, got proxy=%q reset=%v",
			req.Network.ProxyEndpoint, req.Network.ResetUpstreams)
	}
}
