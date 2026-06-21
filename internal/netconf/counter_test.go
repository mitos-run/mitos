package netconf

import (
	"reflect"
	"testing"
)

// TestNftReadEgressCounterArgs asserts the argv reads exactly this sandbox's
// egress counter by name, in JSON so the bytes can be parsed deterministically.
func TestNftReadEgressCounterArgs(t *testing.T) {
	got := NftReadEgressCounterArgs("sbtap0")
	want := []string{"nft", "-j", "list", "counter", "inet", SharedTableName(), SandboxEgressCounterName("sbtap0")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read-counter argv = %v, want %v", got, want)
	}
}

// TestParseEgressCounterBytes parses the byte total out of `nft -j list counter`
// JSON output. The metering pipeline (#211) calls this on the counter read.
func TestParseEgressCounterBytes(t *testing.T) {
	out := `{"nftables":[{"metainfo":{"version":"1.0.6"}},{"counter":{"family":"inet","table":"mitos_egress","name":"sb_sbtap0_egress","handle":4,"packets":12,"bytes":3456}}]}`
	bytes, err := ParseEgressCounterBytes(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bytes != 3456 {
		t.Errorf("bytes = %d, want 3456", bytes)
	}
}

// TestParseEgressCounterBytesNoCounter returns an error when the JSON carries no
// counter object (so a missing counter is not silently read as zero bytes).
func TestParseEgressCounterBytesNoCounter(t *testing.T) {
	out := `{"nftables":[{"metainfo":{"version":"1.0.6"}}]}`
	if _, err := ParseEgressCounterBytes(out); err == nil {
		t.Error("expected an error when the output carries no counter object")
	}
}
