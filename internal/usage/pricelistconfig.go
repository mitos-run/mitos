package usage

import (
	"encoding/json"
	"fmt"
	"strings"
)

// priceListRemediation is the actionable remediation text every
// ParsePriceListConfig error carries: expected shape, the exact allowed keys,
// the unit, and an example. The price list is configuration, never a secret.
const priceListRemediation = "set MITOS_USAGE_PRICELIST (Helm: controller.usage.priceList) to a single JSON object " +
	"whose only keys are vcpu_second, mem_gib_second, storage_gib_hour, egress_gib, and gpu_second, " +
	"each a non-negative number in account-currency dollars per unit, " +
	`for example {"vcpu_second":0.0000128,"egress_gib":0.09}; ` +
	"an omitted key is a zero (free) rate; unset the variable to use the built-in illustrative defaults"

// ParsePriceListConfig parses the MITOS_USAGE_PRICELIST override into a
// PriceList.
//
//   - An empty or all-whitespace value returns DefaultPriceList(): no override
//     configured, the illustrative defaults apply.
//   - A non-empty value must be a single JSON object mapping onto PriceList; it
//     REPLACES the default table entirely (an omitted key is a zero, i.e. free,
//     rate; it never falls back to the default for that key).
//   - Unknown keys, malformed JSON, trailing data, and negative rates are
//     REJECTED with an actionable error so the caller fails startup (fail
//     closed) instead of silently displaying the wrong cost estimate.
//
// Keep this table consistent with the billing rate table (billing.Rates,
// MITOS_CONSOLE_RATES): milli-cents per unit = dollars per unit x 100000. See
// docs/saas/pricing.md.
func ParsePriceListConfig(raw string) (PriceList, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultPriceList(), nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var pl PriceList
	if err := dec.Decode(&pl); err != nil {
		return PriceList{}, fmt.Errorf("invalid price-list JSON: %w; %s", err, priceListRemediation)
	}
	if dec.More() {
		return PriceList{}, fmt.Errorf("invalid price-list JSON: trailing data after the object; %s", priceListRemediation)
	}
	for _, f := range []struct {
		key string
		v   float64
	}{
		{"vcpu_second", pl.VCPUSecond},
		{"mem_gib_second", pl.MemGiBSecond},
		{"storage_gib_hour", pl.StorageGiBHour},
		{"egress_gib", pl.EgressGiB},
		{"gpu_second", pl.GPUSecond},
	} {
		if f.v < 0 {
			return PriceList{}, fmt.Errorf("invalid rate %s=%v: a negative rate is not a price; %s", f.key, f.v, priceListRemediation)
		}
	}
	return pl, nil
}
