package daemon

import (
	"testing"

	"mitos.run/mitos/internal/guestsock"
)

// TestWithSelfServiceEnvAdvertisesSocket proves deliverConfig's env injection
// advertises the in-guest self-service socket and the sandbox's own identity
// (issue #22), allocates a nil input map, and never mutates the caller's map.
func TestWithSelfServiceEnvAdvertisesSocket(t *testing.T) {
	// Nil input: the helper allocates and still advertises the socket + id.
	got := withSelfServiceEnv(nil, "sb-1")
	if got[guestsock.SocketEnvVar] != guestsock.DefaultSocketPath {
		t.Errorf("MITOS_SOCKET = %q, want %q", got[guestsock.SocketEnvVar], guestsock.DefaultSocketPath)
	}
	if got["MITOS_SANDBOX_ID"] != "sb-1" {
		t.Errorf("MITOS_SANDBOX_ID = %q, want sb-1", got["MITOS_SANDBOX_ID"])
	}

	// Caller env survives and is not mutated in place.
	in := map[string]string{"USER_VAR": "v"}
	out := withSelfServiceEnv(in, "sb-2")
	if out["USER_VAR"] != "v" || out["MITOS_SANDBOX_ID"] != "sb-2" {
		t.Errorf("caller env not preserved alongside identity: %+v", out)
	}
	if _, leaked := in[guestsock.SocketEnvVar]; leaked {
		t.Error("withSelfServiceEnv must not mutate the caller's map")
	}

	// A caller-supplied socket path is never overridden.
	custom := withSelfServiceEnv(map[string]string{guestsock.SocketEnvVar: "/run/custom.sock"}, "sb-3")
	if custom[guestsock.SocketEnvVar] != "/run/custom.sock" {
		t.Errorf("caller MITOS_SOCKET overridden: %q", custom[guestsock.SocketEnvVar])
	}
}
