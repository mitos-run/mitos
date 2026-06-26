package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"mitos.run/mitos/internal/apierr"
)

// orgContextKey is the private context key the usage handler reads the caller's
// org from. The org is attached by the #210 gateway after it verifies the
// customer key and resolves the org; the handler NEVER reads the org from a query
// parameter or path, so a request can only ever see its own org's usage.
type orgContextKey struct{}

// WithOrg returns a context carrying orgID. The gateway calls this after key
// verification; the usage handler reads it with OrgFromContext.
func WithOrg(ctx context.Context, orgID string) context.Context {
	return context.WithValue(ctx, orgContextKey{}, orgID)
}

// OrgFromContext returns the org id attached by the gateway and whether one was
// present. A request with no org context is unauthenticated and is refused; it is
// never served as a default org.
func OrgFromContext(ctx context.Context) (string, bool) {
	org, ok := ctx.Value(orgContextKey{}).(string)
	return org, ok && org != ""
}

// PriceList is a simple per-unit rate table used to estimate cost from billable
// units. Rates are in the account currency per unit; a real plan-aware pricing
// engine is a follow-up. Zero rates yield zero cost for that dimension.
type PriceList struct {
	VCPUSecond     float64 `json:"vcpu_second"`
	MemGiBSecond   float64 `json:"mem_gib_second"`
	StorageGiBHour float64 `json:"storage_gib_hour"`
	EgressGiB      float64 `json:"egress_gib"`
	GPUSecond      float64 `json:"gpu_second"`
}

// DefaultPriceList returns illustrative non-zero rates so the usage API returns a
// non-trivial cost out of the box. These are placeholders, not published prices
// (the no-unverified-claims rule). The billing model (#212) reconciles this
// placeholder into the structured billing rate table: billing.FromPriceList
// re-expresses these dollars-per-unit rates as the milli-cents-per-unit
// billing.Rates, so the display-cost estimate here and the real billing rates
// derive from one table. See docs/saas/pricing.md.
func DefaultPriceList() PriceList {
	return PriceList{
		VCPUSecond:     0.0000128,
		MemGiBSecond:   0.0000016,
		StorageGiBHour: 0.0001,
		EgressGiB:      0.09,
		GPUSecond:      0.0006,
	}
}

// Totals is the per-unit roll-up of an org's usage records over the queried
// window.
type Totals struct {
	VCPUSeconds     float64 `json:"vcpu_seconds"`
	MemGiBSeconds   float64 `json:"mem_gib_seconds"`
	StorageGiBHours float64 `json:"storage_gib_hours"`
	EgressBytes     int64   `json:"egress_bytes"`
	GPUSeconds      int64   `json:"gpu_seconds"`
}

// Cost is the estimated cost of the totals under a PriceList, broken out per
// dimension plus the sum.
type Cost struct {
	VCPU    float64 `json:"vcpu"`
	Mem     float64 `json:"mem"`
	Storage float64 `json:"storage"`
	Egress  float64 `json:"egress"`
	GPU     float64 `json:"gpu"`
	Total   float64 `json:"total"`
}

// UsageResponse is the org-scoped usage API payload: the org id (echoed from the
// CONTEXT, never the request), the per-window records, the rolled-up totals, and
// the cost estimate. Both E2B and Daytona lack a real usage API; this per-unit,
// auditable, org-scoped response is a deliberate differentiator.
type UsageResponse struct {
	OrgID   string        `json:"org_id"`
	Records []UsageRecord `json:"records"`
	Totals  Totals        `json:"totals"`
	Cost    Cost          `json:"cost"`
}

// UsageHandler serves an org's current and historical usage and cost. It sits
// BEHIND the #210 gateway: the gateway verifies the key, resolves the org, and
// attaches it to the context, so the handler reads the org from the context and a
// request can only ever read its own org's usage. The optional from/to query
// parameters (RFC3339) bound the window.
type UsageHandler struct {
	store  UsageStore
	prices PriceList
}

// NewUsageHandler builds the usage API handler over a usage store and a price
// list.
func NewUsageHandler(store UsageStore, prices PriceList) *UsageHandler {
	return &UsageHandler{store: store, prices: prices}
}

// ServeHTTP handles GET requests for the calling org's usage. The org is taken
// solely from the request context (attached by the gateway); any org named in the
// query is IGNORED, which is the cross-org isolation guarantee.
func (h *UsageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the usage endpoint serves GET only"))
		return
	}
	orgID, ok := OrgFromContext(r.Context())
	if !ok {
		apierr.Encode(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("no organization context is attached to the request"))
		return
	}

	from, err := parseTime(r.URL.Query().Get("from"))
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the from query parameter is not an RFC3339 timestamp"))
		return
	}
	to, err := parseTime(r.URL.Query().Get("to"))
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the to query parameter is not an RFC3339 timestamp"))
		return
	}

	records, err := h.store.ListRecords(r.Context(), orgID, from, to)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the usage store could not list records"))
		return
	}

	totals := rollUp(records)
	writeUsageJSON(w, UsageResponse{
		OrgID:   orgID,
		Records: records,
		Totals:  totals,
		Cost:    h.prices.cost(totals),
	})
}

// writeUsageJSON writes a UsageResponse as a 200 JSON body. It is shared by the
// public usage API and the internal M2M usage API so both emit the identical
// shape the console decodes.
func writeUsageJSON(w http.ResponseWriter, resp UsageResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// rollUp sums a set of records into per-unit totals.
func rollUp(records []UsageRecord) Totals {
	var t Totals
	for _, r := range records {
		t.VCPUSeconds += r.VCPUSeconds
		t.MemGiBSeconds += r.MemGiBSeconds
		t.StorageGiBHours += r.StorageGiBHours
		t.EgressBytes += r.EgressBytes
		t.GPUSeconds += r.GPUSeconds
	}
	return t
}

// Cost applies the price list to the totals. It is the exported estimator the
// usage API uses internally and the console BFF (issue #214) reuses so the
// console cost view matches the usage API cost exactly.
func (p PriceList) Cost(t Totals) Cost { return p.cost(t) }

// cost applies the price list to the totals.
func (p PriceList) cost(t Totals) Cost {
	egressGiB := float64(t.EgressBytes) / bytesPerGiB
	c := Cost{
		VCPU:    t.VCPUSeconds * p.VCPUSecond,
		Mem:     t.MemGiBSeconds * p.MemGiBSecond,
		Storage: t.StorageGiBHours * p.StorageGiBHour,
		Egress:  egressGiB * p.EgressGiB,
		GPU:     float64(t.GPUSeconds) * p.GPUSecond,
	}
	c.Total = c.VCPU + c.Mem + c.Storage + c.Egress + c.GPU
	return c
}

// parseTime parses an optional RFC3339 timestamp; an empty string is the zero
// time (no bound).
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
