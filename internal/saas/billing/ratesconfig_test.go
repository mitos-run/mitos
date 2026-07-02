package billing

import (
	"strings"
	"testing"
)

// TestParseRatesConfigUnsetUsesDefaults asserts an empty (unset) value falls
// through to the built-in illustrative defaults, so a deployment that sets no
// override behaves exactly as before.
func TestParseRatesConfigUnsetUsesDefaults(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t"} {
		got, err := ParseRatesConfig(raw)
		if err != nil {
			t.Fatalf("ParseRatesConfig(%q) error: %v", raw, err)
		}
		if got != DefaultRates() {
			t.Fatalf("ParseRatesConfig(%q) = %+v, want DefaultRates %+v", raw, got, DefaultRates())
		}
	}
}

// TestParseRatesConfigValidJSONApplies asserts a valid JSON object REPLACES the
// default table entirely: set keys carry the configured value and omitted keys
// are zero (an explicitly free dimension), never the default.
func TestParseRatesConfigValidJSONApplies(t *testing.T) {
	raw := `{"vcpu_second_milli_cents": 2.5, "egress_gib_milli_cents": 12000}`
	got, err := ParseRatesConfig(raw)
	if err != nil {
		t.Fatalf("ParseRatesConfig(%q) error: %v", raw, err)
	}
	want := Rates{VCPUSecondMilliCents: 2.5, EgressGiBMilliCents: 12000}
	if got != want {
		t.Fatalf("ParseRatesConfig = %+v, want %+v (full replacement, omitted keys zero)", got, want)
	}
}

// TestParseRatesConfigFullTable asserts every key round-trips.
func TestParseRatesConfigFullTable(t *testing.T) {
	raw := `{
		"vcpu_second_milli_cents": 1.28,
		"mem_gib_second_milli_cents": 0.16,
		"storage_gib_hour_milli_cents": 10,
		"egress_gib_milli_cents": 9000,
		"gpu_second_milli_cents": 60
	}`
	got, err := ParseRatesConfig(raw)
	if err != nil {
		t.Fatalf("ParseRatesConfig error: %v", err)
	}
	if got != DefaultRates() {
		t.Fatalf("ParseRatesConfig = %+v, want %+v", got, DefaultRates())
	}
}

// TestParseRatesConfigInvalidJSONFailsWithRemediation asserts malformed JSON is
// rejected (fail closed, no silent fallback) and the error carries actionable
// remediation text naming the allowed keys.
func TestParseRatesConfigInvalidJSONFailsWithRemediation(t *testing.T) {
	for _, raw := range []string{"not json", "{", `["vcpu_second_milli_cents"]`, `{"vcpu_second_milli_cents":1.28} trailing`} {
		_, err := ParseRatesConfig(raw)
		if err == nil {
			t.Fatalf("ParseRatesConfig(%q) = nil error, want failure", raw)
		}
		if !strings.Contains(err.Error(), "vcpu_second_milli_cents") {
			t.Fatalf("ParseRatesConfig(%q) error %q lacks remediation text naming the allowed keys", raw, err)
		}
	}
}

// TestParseRatesConfigUnknownFieldFails asserts a typoed key is rejected rather
// than silently ignored, so a misspelled rate never silently becomes free.
func TestParseRatesConfigUnknownFieldFails(t *testing.T) {
	_, err := ParseRatesConfig(`{"vcpu_seconds_milli_cents": 1.28}`)
	if err == nil {
		t.Fatal("ParseRatesConfig with an unknown field returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "vcpu_second_milli_cents") {
		t.Fatalf("unknown-field error %q lacks remediation text naming the allowed keys", err)
	}
}

// TestParseRatesConfigNegativeRateFails asserts a negative rate is rejected: a
// negative price is configuration nonsense that would corrupt the ledger.
func TestParseRatesConfigNegativeRateFails(t *testing.T) {
	_, err := ParseRatesConfig(`{"egress_gib_milli_cents": -1}`)
	if err == nil {
		t.Fatal("ParseRatesConfig with a negative rate returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "egress_gib_milli_cents") {
		t.Fatalf("negative-rate error %q does not name the offending key", err)
	}
}

// TestParseRatesConfigZeroRateIsAllowed asserts an explicit zero is a valid,
// deliberately free dimension (e.g. a self-host deployment with no GPUs).
func TestParseRatesConfigZeroRateIsAllowed(t *testing.T) {
	got, err := ParseRatesConfig(`{"gpu_second_milli_cents": 0}`)
	if err != nil {
		t.Fatalf("ParseRatesConfig with a zero rate error: %v", err)
	}
	if got.GPUSecondMilliCents != 0 {
		t.Fatalf("GPUSecondMilliCents = %v, want 0", got.GPUSecondMilliCents)
	}
}

// TestRatesToPriceListMatchesDefaults asserts ToPriceList (the inverse of
// FromPriceList) re-derives the display price list from the rate table, so a
// console's cost estimate and the billed cost come from ONE configured table.
func TestRatesToPriceListMatchesDefaults(t *testing.T) {
	got := DefaultRates().ToPriceList()
	want := DefaultRates()
	back := FromPriceList(got)
	fields := []struct {
		name       string
		have, wanb float64
	}{
		{"VCPUSecondMilliCents", back.VCPUSecondMilliCents, want.VCPUSecondMilliCents},
		{"MemGiBSecondMilliCents", back.MemGiBSecondMilliCents, want.MemGiBSecondMilliCents},
		{"StorageGiBHourMilliCents", back.StorageGiBHourMilliCents, want.StorageGiBHourMilliCents},
		{"EgressGiBMilliCents", back.EgressGiBMilliCents, want.EgressGiBMilliCents},
		{"GPUSecondMilliCents", back.GPUSecondMilliCents, want.GPUSecondMilliCents},
	}
	for _, f := range fields {
		diff := f.have - f.wanb
		if diff < 0 {
			diff = -diff
		}
		if f.wanb != 0 && diff/f.wanb > 1e-12 {
			t.Fatalf("round trip %s = %v, want %v", f.name, f.have, f.wanb)
		}
	}
}
