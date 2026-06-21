//go:build linux

package main

import (
	"fmt"
	"net"
	"os"

	"mitos.run/mitos/internal/guestsock"
)

// startSelfServiceSocket serves the in-guest self-service endpoint (issue #22,
// API v2 section 2.2) on a unix socket inside the VM. The in-VM workload (and
// the mitos.guest SDK) connects to it via the MITOS_SOCKET env var to read its
// own identity and budget without any network egress and without an external
// orchestrator round-trip.
//
// The socket path comes from MITOS_SOCKET (the host advertises it at claim
// time) or guestsock.DefaultSocketPath (/run/mitos.sock, on the tmpfs the agent
// mounts at /run). Identity and budget are read from the env the host delivered
// via configure (the configuredEnv map), so the response carries names and
// numbers only, never a secret VALUE.
//
// It runs in its own goroutine so it never blocks the vsock accept loop. A
// listen failure is logged and non-fatal: the self-service socket is an
// optional convenience, not a load-bearing path, so a sandbox without it still
// serves exec/files over vsock as before.
func startSelfServiceSocket() {
	path := os.Getenv(guestsock.SocketEnvVar)
	if path == "" {
		path = guestsock.DefaultSocketPath
	}

	// A stale socket from a pre-fork snapshot would block bind; remove it first.
	// Best effort: a missing file is fine, and a real bind error surfaces below.
	_ = os.Remove(path)

	lis, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-agent: self-service socket listen %s: %v\n", path, err)
		return
	}

	h := guestsock.Handler{
		// Read identity and budget from the delivered env first, falling back to
		// the process env. The configuredEnv map holds claim-time env+secrets;
		// only the non-secret MITOS_ identity/budget keys are surfaced by the
		// handler, which whitelists the keys it reads.
		Env: func(key string) (string, bool) {
			configuredMu.Lock()
			v, ok := configuredEnv[key]
			configuredMu.Unlock()
			if ok {
				return v, true
			}
			return os.LookupEnv(key)
		},
		// Fork is left nil: budget-gated self-fork is continuation (issue #25).
		// The handler answers a fork request with the not-enabled escalation path
		// so the socket and the SDK surface are real and exercised today.
		Fork: nil,
	}

	fmt.Println("sandbox-agent: self-service socket ready on", path)
	go func() {
		if err := h.Serve(lis); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox-agent: self-service socket serve: %v\n", err)
		}
	}()
}
