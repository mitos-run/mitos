package console

import (
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// setSpendCapRequest is the body of POST /console/billing/spend-cap.
// Money values are integer cents; floats are never accepted.
type setSpendCapRequest struct {
	SoftCents int64 `json:"soft_cents"`
	HardCents int64 `json:"hard_cents"`
}

// handleSetSpendCap sets the org's spend cap. It requires billing.manage so
// only owners and billing-role members can change the cap; a viewer or member
// cannot. A zero value for either threshold means "not set" for that level.
// Never logs the cent amounts beyond the org id.
func (c *Console) handleSetSpendCap(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.authorize(r, saas.PermManageBilling)
	if !ok {
		apierr.Encode(w, e)
		return
	}

	if c.deps.Billing.Caps == nil {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("spend cap management is not available in this edition"))
		return
	}

	var req setSpendCapRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the spend-cap body is not valid JSON"))
		return
	}

	if req.SoftCents < 0 || req.HardCents < 0 {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("soft_cents and hard_cents must not be negative"))
		return
	}
	if req.SoftCents > 0 && req.HardCents > 0 && req.SoftCents > req.HardCents {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("soft_cents must not exceed hard_cents"))
		return
	}

	cap := billing.SpendCap{
		OrgID:   orgID,
		SoftCap: billing.Money(req.SoftCents),
		HardCap: billing.Money(req.HardCents),
	}
	if err := c.deps.Billing.Caps.Set(r.Context(), cap); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the spend cap could not be saved"))
		return
	}

	// Log org id only; never log cent amounts or secrets.
	c.deps.Log.Info("spend cap updated", "org", orgID)

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id": orgID,
	})
}
