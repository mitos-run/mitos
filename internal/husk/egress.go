package husk

import (
	"os/exec"

	"mitos.run/mitos/internal/netconf"
)

// readEgressCounterBytes is the production per-tap egress byte reader for the
// husk pod's Metering: it runs netconf.NftReadEgressCounterArgs(tap) in the pod
// netns (the stub's own netns, so a plain exec suffices) and parses the JSON
// with netconf.ParseEgressCounterBytes. Any failure (nft absent, counter not
// yet installed, parse error) reads as 0: metering degrades to zero egress, it
// never errors the report. This mirrors the fork engine's reader; the argv and
// parser are unit-tested in internal/netconf, so this thin wrapper's real
// exercise is in KVM CI where nft exists.
func readEgressCounterBytes(tap string) int64 {
	argv := netconf.NftReadEgressCounterArgs(tap)
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		return 0
	}
	bytes, err := netconf.ParseEgressCounterBytes(string(out))
	if err != nil {
		return 0
	}
	return bytes
}
