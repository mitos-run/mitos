package main

import (
	"context"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

// TestStubControlPlaneEchoesOrgAndOp asserts the binary's default forward target
// echoes the resolved org and op so a smoke test can confirm authentication and
// org-resolution worked. It never fabricates a sandbox.
func TestStubControlPlaneEchoesOrgAndOp(t *testing.T) {
	resp, err := stubControlPlane{}.Forward(context.Background(), saas.ForwardRequest{OrgID: "org-a", Op: "sandbox.create"})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	body := string(resp.Body)
	if !strings.Contains(body, `"org":"org-a"`) || !strings.Contains(body, `"op":"sandbox.create"`) {
		t.Errorf("forward body %q missing echoed org/op", body)
	}
}

// TestStubControlPlaneCarriesOnlyVerifiedOrg asserts the stub forwards exactly
// the org it was handed, so a gateway that attaches org A can never have the stub
// act on org B.
func TestStubControlPlaneCarriesOnlyVerifiedOrg(t *testing.T) {
	resp, err := stubControlPlane{}.Forward(context.Background(), saas.ForwardRequest{OrgID: "org-a", Op: "sandbox.create", Body: []byte(`{"org":"org-b"}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if strings.Contains(string(resp.Body), "org-b") {
		t.Errorf("stub echoed org-b from the body; only the verified org may appear: %s", resp.Body)
	}
}
