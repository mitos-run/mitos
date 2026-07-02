package billing

import (
	"encoding/json"
	"fmt"
	"strings"

	"mitos.run/mitos/internal/usage"
)

// ratesRemediation is the actionable remediation text every ParseRatesConfig
// error carries (the LLM-legible error rule): what shape is expected, the exact
// allowed keys, the unit, and an example. It never echoes any secret; the rate
// table itself is configuration, not a secret.
const ratesRemediation = "set MITOS_CONSOLE_RATES (Helm: console.billing.rates) to a single JSON object " +
	"whose only keys are vcpu_second_milli_cents, mem_gib_second_milli_cents, " +
	"storage_gib_hour_milli_cents, egress_gib_milli_cents, and gpu_second_milli_cents, " +
	"each a non-negative number in MILLI-cents (thousandths of a cent) per unit, " +
	`for example {"vcpu_second_milli_cents":1.28,"egress_gib_milli_cents":9000}; ` +
	"an omitted key is a zero (free) rate; unset the variable to use the built-in illustrative defaults"

// ParseRatesConfig parses the MITOS_CONSOLE_RATES override into a Rates table.
//
//   - An empty or all-whitespace value returns DefaultRates(): no override
//     configured, the illustrative defaults apply.
//   - A non-empty value must be a single JSON object mapping onto Rates; it
//     REPLACES the default table entirely (an omitted key is a zero, i.e. free,
//     rate; it never falls back to the default for that key).
//   - Unknown keys, malformed JSON, trailing data, and negative rates are
//     REJECTED with an actionable error so the caller fails startup (fail
//     closed) instead of silently billing at the wrong rates.
func ParseRatesConfig(raw string) (Rates, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultRates(), nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Rates
	if err := dec.Decode(&r); err != nil {
		return Rates{}, fmt.Errorf("invalid rates JSON: %w; %s", err, ratesRemediation)
	}
	if dec.More() {
		return Rates{}, fmt.Errorf("invalid rates JSON: trailing data after the object; %s", ratesRemediation)
	}
	for _, f := range []struct {
		key string
		v   float64
	}{
		{"vcpu_second_milli_cents", r.VCPUSecondMilliCents},
		{"mem_gib_second_milli_cents", r.MemGiBSecondMilliCents},
		{"storage_gib_hour_milli_cents", r.StorageGiBHourMilliCents},
		{"egress_gib_milli_cents", r.EgressGiBMilliCents},
		{"gpu_second_milli_cents", r.GPUSecondMilliCents},
	} {
		if f.v < 0 {
			return Rates{}, fmt.Errorf("invalid rate %s=%v: a negative rate would corrupt the credit ledger; %s", f.key, f.v, ratesRemediation)
		}
	}
	return r, nil
}

// ToPriceList re-expresses the milli-cents-per-unit Rates as the
// dollars-per-unit usage.PriceList: the inverse of FromPriceList. It lets the
// display-cost estimate (the usage API's PriceList) derive from the SAME
// configured rate table the ledger bills with, so the number a user sees and
// the number they are charged never drift.
func (r Rates) ToPriceList() usage.PriceList {
	const milliCentsPerDollar = 100000.0 // $1 = 100 cents = 100000 milli-cents.
	return usage.PriceList{
		VCPUSecond:     r.VCPUSecondMilliCents / milliCentsPerDollar,
		MemGiBSecond:   r.MemGiBSecondMilliCents / milliCentsPerDollar,
		StorageGiBHour: r.StorageGiBHourMilliCents / milliCentsPerDollar,
		EgressGiB:      r.EgressGiBMilliCents / milliCentsPerDollar,
		GPUSecond:      r.GPUSecondMilliCents / milliCentsPerDollar,
	}
}
