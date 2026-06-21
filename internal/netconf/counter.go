package netconf

import (
	"encoding/json"
	"fmt"
)

// This file holds the read side of the per-sandbox egress byte counter: the
// argv that reads the counter and the parser for its JSON output. The metering
// pipeline (#211) reads the counter by name to attribute egress bytes to a
// sandbox. Parsing JSON output keeps the read deterministic and unit-testable on
// any platform; the actual `nft` invocation is Linux-only.

// NftReadEgressCounterArgs builds the argv to read this sandbox's egress counter
// in JSON: nft -j list counter inet <table> sb_<tap>_egress. The JSON form is
// parsed by ParseEgressCounterBytes; the metering pipeline pairs the two to read
// per-sandbox egress bytes.
func NftReadEgressCounterArgs(tap string) []string {
	return []string{"nft", "-j", "list", "counter", "inet", SharedTableName(), SandboxEgressCounterName(tap)}
}

// nftCounterJSON is the minimal shape of `nft -j list counter` output: a top
// "nftables" array whose elements may carry a "counter" object. Only the bytes
// field is consumed.
type nftCounterJSON struct {
	Nftables []struct {
		Counter *struct {
			Bytes   int64 `json:"bytes"`
			Packets int64 `json:"packets"`
		} `json:"counter"`
	} `json:"nftables"`
}

// ParseEgressCounterBytes extracts the byte total from `nft -j list counter`
// JSON output. It returns an error when the output carries no counter object so
// a missing or never-installed counter is never silently read as zero bytes (the
// caller decides whether absence means zero or a real read failure).
func ParseEgressCounterBytes(jsonOut string) (int64, error) {
	var parsed nftCounterJSON
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		return 0, fmt.Errorf("parse nft counter json: %w", err)
	}
	for _, e := range parsed.Nftables {
		if e.Counter != nil {
			return e.Counter.Bytes, nil
		}
	}
	return 0, fmt.Errorf("nft counter json carried no counter object")
}
