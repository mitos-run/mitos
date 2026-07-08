package netconf

import (
	"fmt"
	"net"
	"strings"
)

// This file holds pure argv builders for the host networking commands. No
// exec happens here so the argument shapes are unit testable on any platform;
// the Linux-tagged internal/network package feeds these to a real runner.

// TapAddArgs builds the argv to create a tap device:
// ip tuntap add <tap> mode tap.
func TapAddArgs(tap string) []string {
	return []string{"ip", "tuntap", "add", tap, "mode", "tap"}
}

// AddrAddArgs builds the argv to assign the host side of the per-sandbox /30
// to the tap: ip addr add <hostIP>/30 dev <tap>.
func AddrAddArgs(hostIP net.IP, tap string) []string {
	return []string{"ip", "addr", "add", fmt.Sprintf("%s/30", hostIP.String()), "dev", tap}
}

// LinkUpArgs builds the argv to bring the tap up: ip link set <tap> up.
func LinkUpArgs(tap string) []string {
	return []string{"ip", "link", "set", tap, "up"}
}

// ResolverAddrAddArgs builds the argv to bind the in-pod DNS resolver address to
// the tap as a /32: ip addr add <resolverIP>/32 dev <tap>. The husk per-pod DNS
// proxy listens on this address, and the guest's queries (routed to it via the
// tap gateway) must be delivered LOCALLY rather than forwarded out, so the
// address has to be local in the pod netns. Binding it on the tap (the interface
// the guest's packets arrive on) achieves both.
func ResolverAddrAddArgs(resolverIP net.IP, tap string) []string {
	return []string{"ip", "addr", "add", fmt.Sprintf("%s/32", resolverIP.String()), "dev", tap}
}

// LinkDelArgs builds the argv to remove the tap: ip link del <tap>.
func LinkDelArgs(tap string) []string {
	return []string{"ip", "link", "del", tap}
}

// IPBatchArgs builds the argv to run several ip subcommands from stdin in ONE
// process: ip -batch -. Each stdin line is one ip subcommand WITHOUT the
// leading "ip" (see RenderIPBatch). Batch mode aborts on the first failing line
// and exits non-zero, so a partially applied tap setup fails the whole call
// (fail-closed): the caller tears the tap down on any error. This replaces the
// ~4 separate fork+exec of ip on the fork hot path with a single one.
func IPBatchArgs() []string {
	return []string{"ip", "-batch", "-"}
}

// RenderIPBatch renders the newline-delimited command list fed to `ip -batch -`
// (IPBatchArgs) that brings this VM's tap up: create the tap, assign the host
// side of the /30, set the link up, and (single-VM DNS path) bind the in-pod
// resolver /32. Each line is the argv of the matching single-command builder
// (TapAddArgs, AddrAddArgs, LinkUpArgs, ResolverAddrAddArgs) with the leading
// "ip" stripped, so the batched and unbatched forms share one source of truth
// and cannot drift. The idempotent pre-create tap delete is intentionally NOT
// batched here: its "not found" is the normal case and must not abort the batch.
func RenderIPBatch(tap string, hostIP, resolverIP net.IP) string {
	cmds := [][]string{
		TapAddArgs(tap),
		AddrAddArgs(hostIP, tap),
		LinkUpArgs(tap),
	}
	if resolverIP != nil {
		cmds = append(cmds, ResolverAddrAddArgs(resolverIP, tap))
	}
	var b strings.Builder
	for _, c := range cmds {
		// Drop the leading "ip": ip -batch reads bare subcommands, one per line.
		b.WriteString(strings.Join(c[1:], " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// NftApplyArgs builds the argv to apply a rendered ruleset from stdin:
// nft -f -. The caller pipes a rendered ruleset (RenderSharedTable or
// RenderSandboxChain) on stdin.
func NftApplyArgs() []string {
	return []string{"nft", "-f", "-"}
}

// NftDeleteDispatchElementArgs builds the argv to remove this tap's dispatch
// element from the shared verdict map: nft delete element inet <table> <map>
// { "<tap>" }. After this no traffic jumps into the sandbox chain, so the
// chain can be removed. Deleting by key needs no rule handle.
func NftDeleteDispatchElementArgs(tap string) []string {
	return []string{"nft", "delete", "element", "inet", SharedTableName(), DispatchMapName(),
		fmt.Sprintf("{ %q }", tap)}
}

// NftDeleteSandboxChainArgs builds the argv to remove this sandbox's regular
// chain from the shared table: nft delete chain inet <table> sb_<tap>. The
// shared table, base chain, and map are left intact for other sandboxes.
func NftDeleteSandboxChainArgs(tap string) []string {
	return []string{"nft", "delete", "chain", "inet", SharedTableName(), SandboxChainName(tap)}
}

// NftDeleteInputDispatchElementArgs builds the argv to remove this tap's element
// from the INPUT dispatch map: nft delete element inet <table> <inmap>
// { "<tap>" }. After this no input traffic jumps into the sandbox input chain,
// so that chain can be removed. The husk-path counterpart of
// NftDeleteDispatchElementArgs.
func NftDeleteInputDispatchElementArgs(tap string) []string {
	return []string{"nft", "delete", "element", "inet", SharedTableName(), InputDispatchMapName(),
		fmt.Sprintf("{ %q }", tap)}
}

// NftDeleteSandboxInputChainArgs builds the argv to remove this sandbox's input
// chain: nft delete chain inet <table> sbin_<tap>. Run after the input dispatch
// element is deleted. The shared table, input base chain, and input map are left
// intact.
func NftDeleteSandboxInputChainArgs(tap string) []string {
	return []string{"nft", "delete", "chain", "inet", SharedTableName(), SandboxInputChainName(tap)}
}

// NftDeleteSandboxAllowSetArgs builds the argv to remove this sandbox's dynamic
// allow set from the shared table: nft delete set inet <table> sb_<tap>_dyn.
// This must run after the per-sandbox chain is deleted, because the chain's
// accept rule references the set. Deleting it ensures a tap reused for a later
// sandbox starts with no stale pinned (ip . port) elements.
func NftDeleteSandboxAllowSetArgs(tap string) []string {
	return []string{"nft", "delete", "set", "inet", SharedTableName(), SandboxAllowSetName(tap)}
}

// NftDeleteSandboxEgressCounterArgs builds the argv to remove this sandbox's
// egress counter from the shared table: nft delete counter inet <table>
// sb_<tap>_egress. Run after the per-sandbox chain is deleted (the chain's
// counting rule references the counter), so a tap reused for a later sandbox
// starts with a fresh zeroed counter and never inherits stale byte totals.
func NftDeleteSandboxEgressCounterArgs(tap string) []string {
	return []string{"nft", "delete", "counter", "inet", SharedTableName(), SandboxEgressCounterName(tap)}
}

// NftDeleteProxyDNATDispatchElementArgs builds the argv to remove this tap's
// element from the proxy DNAT dispatch map: nft delete element ip <nattable>
// proxydnat { "<tap>" }. After this no prerouting traffic jumps into the per-tap
// DNAT chain, so the chain can be removed. The nat-path counterpart of
// NftDeleteDispatchElementArgs; deleting by key needs no rule handle.
func NftDeleteProxyDNATDispatchElementArgs(tap string) []string {
	return []string{"nft", "delete", "element", "ip", NatTableName(), ProxyDNATDispatchMapName(),
		fmt.Sprintf("{ %q }", tap)}
}

// NftDeleteProxyDNATChainArgs builds the argv to remove this tap's DNAT chain
// from the nat table: nft delete chain ip <nattable> proxydnat_<tap>. Run after
// the dispatch element is deleted (the element references the chain). The nat
// table, base chain, and dispatch map are left intact for other forks. This
// stops a reused tap from inheriting a stale sentinel DNAT and keeps the
// prerouting dispatch from growing unbounded.
func NftDeleteProxyDNATChainArgs(tap string) []string {
	return []string{"nft", "delete", "chain", "ip", NatTableName(), ProxyDNATChainName(tap)}
}

// MasqueradeAddArgs builds the argv to add a MASQUERADE rule for the sandbox
// subnet on the uplink interface. This is optional (the node may already NAT
// the subnet); callers gate it behind a flag.
func MasqueradeAddArgs(subnetCIDR, uplink string) []string {
	return []string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnetCIDR, "-o", uplink, "-j", "MASQUERADE"}
}

// MasqueradeDelArgs builds the argv to remove the MASQUERADE rule added by
// MasqueradeAddArgs.
func MasqueradeDelArgs(subnetCIDR, uplink string) []string {
	return []string{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnetCIDR, "-o", uplink, "-j", "MASQUERADE"}
}
