package daemon

import "testing"

// The requested-timeout ceiling (issue #216) is still enforced for the live
// lifecycle route /v1/set_timeout; that determinism guarantee (a request over the
// ceiling is REJECTED with timeout_too_large, never silently clamped) is asserted
// by TestSetTimeoutOverCeilingIsRejected in lifecycle_api_test.go. The legacy
// JSON /v1/exec, /v1/exec/stream, and /v1/run_code/stream wires that once carried
// the exec-side ceiling and the exec-timeout-124 envelope were removed in #358;
// the runtime exec/run_code surface is now the Connect sandbox.v1.Sandbox
// protocol, which forwards the requested timeout to the guest (the guest enforces
// its own execution deadline and reports the conventional 124 exit code on the
// terminal frame, which the SDKs map to the typed exec_timeout error).

// TestDefaultCeilingIs24Hours asserts the documented default ceiling so the
// exec_background default (86400s) is honored out of the box.
func TestDefaultCeilingIs24Hours(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	if got := api.maxExecTimeout; got != defaultMaxExecTimeoutSeconds {
		t.Fatalf("default ceiling = %d, want %d", got, defaultMaxExecTimeoutSeconds)
	}
	if defaultMaxExecTimeoutSeconds != 86400 {
		t.Fatalf("documented default ceiling is 86400s (24h), got %d", defaultMaxExecTimeoutSeconds)
	}
}
