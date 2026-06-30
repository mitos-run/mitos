// Package network applies and tears down per-sandbox host networking: a tap
// device, its host IP, and a per-tap nftables egress ruleset. The real exec
// path is Linux-only and behind a build tag, but the orchestration logic (the
// sequence of host commands) lives in this platform-independent file with an
// injected runner so it is unit-testable on any platform without root.
package network

import (
	"context"
	"net"
	"sync"

	"mitos.run/mitos/internal/netconf"
)

// Manager applies and removes the host-side network for a sandbox identity.
type Manager interface {
	// Setup creates the tap, assigns the host IP, brings the link up, and
	// installs the per-tap egress ruleset for the given network policy
	// (egress default, allowlists, block_network, CIDR allowlist, and the
	// optional egress byte counter). The inbound policy is applied on the husk
	// input path; the raw-forkd host path runs no input hook.
	Setup(ctx context.Context, id netconf.Identity, policy netconf.SandboxPolicy, resolverIP net.IP) error
	// Teardown removes the tap and the per-tap nftables table.
	Teardown(ctx context.Context, id netconf.Identity) error
	// FlushSource deletes all conntrack entries whose source IP is guestIP.
	// This is called on a live fork so that any in-flight proxied flow is
	// RST'd and the child re-dials through its own proxy context. Returns nil
	// when no entries matched (conntrack exits nonzero but that is not an
	// error for the caller).
	FlushSource(ctx context.Context, guestIP net.IP) error
}

// FakeManager records Setup, Teardown, and FlushSource calls for use by other
// packages' tests. It performs no real work and is safe for concurrent use.
type FakeManager struct {
	mu           sync.Mutex
	SetupLog     []SetupCall
	Teardowns    []netconf.Identity
	FlushSources []net.IP
	SetupErr     error
	TeardownErr  error
	FlushErr     error
}

// SetupCall records the arguments of one Setup call.
type SetupCall struct {
	Identity   netconf.Identity
	Policy     netconf.SandboxPolicy
	ResolverIP net.IP
}

// Setup records the call and returns FakeManager.SetupErr.
func (f *FakeManager) Setup(_ context.Context, id netconf.Identity, policy netconf.SandboxPolicy, resolverIP net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SetupLog = append(f.SetupLog, SetupCall{Identity: id, Policy: policy, ResolverIP: resolverIP})
	return f.SetupErr
}

// Teardown records the call and returns FakeManager.TeardownErr.
func (f *FakeManager) Teardown(_ context.Context, id netconf.Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Teardowns = append(f.Teardowns, id)
	return f.TeardownErr
}

// FlushSource records the call and returns FakeManager.FlushErr.
func (f *FakeManager) FlushSource(_ context.Context, guestIP net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FlushSources = append(f.FlushSources, guestIP)
	return f.FlushErr
}

var _ Manager = (*FakeManager)(nil)
