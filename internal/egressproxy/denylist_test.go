package egressproxy

import (
	"net"
	"testing"
)

// TestIsDeniedIP asserts the exported denied-IP floor (consumed by the SNI proxy
// on its transparent splice path) refuses cloud metadata, loopback, link-local,
// the unspecified address, and the NAT64 wrap of IMDS, while allowing a public
// destination. A nil IP is denied (fail closed).
func TestIsDeniedIP(t *testing.T) {
	denied := []string{
		"169.254.169.254",    // AWS/GCP IMDS
		"127.0.0.1",          // loopback: forkd gRPC/sandbox API
		"0.0.0.0",            // unspecified (routes to loopback on Linux)
		"::1",                // IPv6 loopback
		"fe80::1",            // IPv6 link-local
		"fd00:ec2::254",      // AWS IMDSv6
		"64:ff9b::a9fe:a9fe", // NAT64 of 169.254.169.254
	}
	for _, s := range denied {
		if !IsDeniedIP(net.ParseIP(s)) {
			t.Errorf("IsDeniedIP(%s) = false, want true (must be denied)", s)
		}
	}

	allowed := []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if IsDeniedIP(net.ParseIP(s)) {
			t.Errorf("IsDeniedIP(%s) = true, want false (public, must be allowed)", s)
		}
	}

	if !IsDeniedIP(nil) {
		t.Error("IsDeniedIP(nil) = false, want true (fail closed)")
	}
}
