//go:build linux

package network

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"

	"mitos.run/mitos/internal/netconf"
)

// flushRunner runs argv and returns the combined stdout+stderr output alongside
// the process error. It is a separate seam from runner because FlushSource
// needs to inspect the output text to distinguish "no entries matched" (exit 1
// with a deletion summary) from a genuine failure (exit 1 with, e.g.,
// "Operation not permitted").
type flushRunner func(ctx context.Context, argv []string) (string, error)

// execFlushRunner is the production flushRunner: it captures combined
// stdout+stderr so FlushSource can match on the output message.
func execFlushRunner(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// linuxManager is the real, root-requiring Manager. It wires a real
// exec-based runner into the platform-independent setup/teardown
// orchestration in network_apply.go. The orchestration is unit-tested with a
// fake runner; this file is exercised end to end only in KVM CI.
type linuxManager struct {
	run     runner
	flush   flushRunner
	enableF forwardEnabler
	opts    applyOptions
}

// Options configures the Linux network Manager.
type Options struct {
	// SubnetCIDR is the sandbox subnet, used for the optional MASQUERADE rule.
	SubnetCIDR string
	// Uplink is the host egress interface. When empty no MASQUERADE is added
	// and the node's existing NAT is relied upon (documented default).
	Uplink string
	// EnableForwarding writes 1 to /proc/sys/net/ipv4/ip_forward on each
	// Setup. Default false: the node is assumed to already forward, or NAT is
	// handled upstream.
	EnableForwarding bool
	// ProxyEnabled mirrors the node-wide egress proxy flag so teardown removes
	// each tap's per-fork prerouting DNAT. Default false leaves the proxy
	// datapath untouched, exactly as before.
	ProxyEnabled bool
}

// NewManager builds the Linux network Manager with a real exec runner.
func NewManager(opts Options) Manager {
	return &linuxManager{
		run:     execRunner,
		flush:   execFlushRunner,
		enableF: enableIPForward,
		opts: applyOptions{
			subnetCIDR:       opts.SubnetCIDR,
			uplink:           opts.Uplink,
			enableForwarding: opts.EnableForwarding,
			proxyEnabled:     opts.ProxyEnabled,
		},
	}
}

func (m *linuxManager) Setup(ctx context.Context, id netconf.Identity, policy netconf.SandboxPolicy, resolverIP net.IP) error {
	return setup(ctx, m.run, m.enableF, id, policy, resolverIP, m.opts)
}

func (m *linuxManager) Teardown(ctx context.Context, id netconf.Identity) error {
	return teardown(ctx, m.run, id, m.opts)
}

// execRunner runs argv via exec.CommandContext, feeding stdin when non-empty.
// On failure it includes captured stderr so the error is actionable; argv and
// stderr from these host tools do not carry secrets.
func execRunner(ctx context.Context, argv []string, stdin string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(stdin))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("%s: %w: %s", argv[0], err, stderr.String())
		}
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// enableIPForward writes 1 to /proc/sys/net/ipv4/ip_forward.
func enableIPForward() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("write ip_forward: %w", err)
	}
	return nil
}

var _ Manager = (*linuxManager)(nil)
