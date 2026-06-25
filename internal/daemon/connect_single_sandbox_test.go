package daemon

import "testing"

// TestConnectLookupTokenResolvesSingleSandbox asserts that the Connect bearer
// gate resolves single-sandbox (husk-stub) mode the same way the legacy JSON
// gate did: the cluster SDK addresses the in-pod API with the claim's sandbox id
// (the husk pod name), which never equals the stub's fixed local id, so the
// token registered under the local id must still authorize the SDK's request.
// Without this, cluster-mode exec over Connect against a husk pod would 401.
func TestConnectLookupTokenResolvesSingleSandbox(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.SetSingleSandbox("husk-local")
	api.RegisterToken("husk-local", "per-sandbox-bearer")

	// The SDK sends the husk pod name, not the stub's local id.
	got, ok := api.connectLookupToken("mitos-py-husk-5gwmh")
	if !ok || got != "per-sandbox-bearer" {
		t.Fatalf("single-sandbox connect token lookup: got (%q,%v), want (per-sandbox-bearer,true)", got, ok)
	}
}

// TestConnectLookupTokenMultiSandboxStaysStrict asserts that in forkd's default
// multi-sandbox mode the Connect gate is unchanged: a token for sandbox A cannot
// authorize sandbox B (no cross-sandbox resolution).
func TestConnectLookupTokenMultiSandboxStaysStrict(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.RegisterToken("sandbox-a", "token-a")

	if _, ok := api.connectLookupToken("sandbox-b"); ok {
		t.Fatal("multi-sandbox connect token lookup must not resolve a different sandbox id")
	}
	if got, ok := api.connectLookupToken("sandbox-a"); !ok || got != "token-a" {
		t.Fatalf("multi-sandbox connect token lookup for the right id: got (%q,%v), want (token-a,true)", got, ok)
	}
}
