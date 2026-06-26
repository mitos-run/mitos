package usage

import (
	"crypto/subtle"
	"net/http"

	"mitos.run/mitos/internal/apierr"
)

// InternalUsageHandler serves an org's usage over a MACHINE-TO-MACHINE endpoint
// the controller mounts so a separate process (the console) can read the SAME
// usage the controller's collector recorded, without a shared database. Unlike
// the public UsageHandler (which reads the org from the gateway-attached
// context), this endpoint is bearer-gated by a shared secret and takes the org
// from InternalOrgHeader: its only caller is the console, which already derived
// the org from the gateway-verified session, so the org in the header is a
// trusted upstream fact, exactly like the identity-resolve endpoint.
//
// SECURITY: the bearer token is compared in constant time and is never logged.
// The org is still scoped at the store: ListRecords returns only the named org's
// records, so even a malformed header can only ever return that org's data, never
// a cross-org bleed. A missing/empty org or a bad token is refused.
type InternalUsageHandler struct {
	store  UsageStore
	prices PriceList
	token  string
}

// NewInternalUsageHandler builds the M2M usage handler over a usage store, a
// price list, and the shared bearer token. An empty token disables the endpoint
// (every request is refused) so a misconfiguration fails closed rather than
// serving usage unauthenticated.
func NewInternalUsageHandler(store UsageStore, prices PriceList, token string) *InternalUsageHandler {
	return &InternalUsageHandler{store: store, prices: prices, token: token}
}

// ServeHTTP authenticates the bearer token, reads the org from InternalOrgHeader,
// and serves that org's usage. It returns the same UsageResponse shape the public
// usage API does so the console can reuse one decoder.
func (h *InternalUsageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the internal usage endpoint serves GET only"))
		return
	}
	if h.token == "" || !bearerEquals(r.Header.Get("Authorization"), h.token) {
		apierr.Encode(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("the internal usage endpoint requires a valid bearer token"))
		return
	}
	orgID := r.Header.Get(InternalOrgHeader)
	if orgID == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("the internal usage request carries no organization header"))
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

// bearerEquals reports whether the Authorization header carries exactly
// "Bearer <token>", comparing the token in constant time. A missing prefix or a
// length mismatch is a non-match without leaking timing.
func bearerEquals(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	got := header[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
