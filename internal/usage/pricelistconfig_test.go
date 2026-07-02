package usage

import (
	"strings"
	"testing"
)

// TestParsePriceListConfigUnsetUsesDefaults asserts an empty (unset) value falls
// through to the illustrative defaults, so a deployment with no override
// behaves exactly as before.
func TestParsePriceListConfigUnsetUsesDefaults(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := ParsePriceListConfig(raw)
		if err != nil {
			t.Fatalf("ParsePriceListConfig(%q) error: %v", raw, err)
		}
		if got != DefaultPriceList() {
			t.Fatalf("ParsePriceListConfig(%q) = %+v, want DefaultPriceList %+v", raw, got, DefaultPriceList())
		}
	}
}

// TestParsePriceListConfigValidJSONApplies asserts a valid JSON object REPLACES
// the default table entirely: omitted keys are zero (free), never the default.
func TestParsePriceListConfigValidJSONApplies(t *testing.T) {
	raw := `{"vcpu_second": 0.00002, "egress_gib": 0.12}`
	got, err := ParsePriceListConfig(raw)
	if err != nil {
		t.Fatalf("ParsePriceListConfig(%q) error: %v", raw, err)
	}
	want := PriceList{VCPUSecond: 0.00002, EgressGiB: 0.12}
	if got != want {
		t.Fatalf("ParsePriceListConfig = %+v, want %+v (full replacement, omitted keys zero)", got, want)
	}
}

// TestParsePriceListConfigInvalidJSONFails asserts malformed JSON is rejected
// (fail closed) and the error carries remediation text naming the allowed keys.
func TestParsePriceListConfigInvalidJSONFails(t *testing.T) {
	for _, raw := range []string{"not json", "{", `{"vcpu_second":0.1} x`} {
		_, err := ParsePriceListConfig(raw)
		if err == nil {
			t.Fatalf("ParsePriceListConfig(%q) = nil error, want failure", raw)
		}
		if !strings.Contains(err.Error(), "vcpu_second") {
			t.Fatalf("ParsePriceListConfig(%q) error %q lacks remediation text naming the allowed keys", raw, err)
		}
	}
}

// TestParsePriceListConfigUnknownFieldFails asserts a typoed key is rejected
// rather than silently ignored.
func TestParsePriceListConfigUnknownFieldFails(t *testing.T) {
	_, err := ParsePriceListConfig(`{"vcpu_seconds": 0.1}`)
	if err == nil {
		t.Fatal("ParsePriceListConfig with an unknown field returned nil error, want failure")
	}
}

// TestParsePriceListConfigNegativeRateFails asserts a negative display rate is
// rejected as configuration nonsense.
func TestParsePriceListConfigNegativeRateFails(t *testing.T) {
	_, err := ParsePriceListConfig(`{"gpu_second": -0.5}`)
	if err == nil {
		t.Fatal("ParsePriceListConfig with a negative rate returned nil error, want failure")
	}
	if !strings.Contains(err.Error(), "gpu_second") {
		t.Fatalf("negative-rate error %q does not name the offending key", err)
	}
}
