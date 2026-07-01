package console

import (
	"context"
	"fmt"
	"net/http"

	"mitos.run/mitos/internal/apierr"
)

// FirstActivityView is the wire shape for GET /console/first-activity.
// Active is true when the org has at least one fork served or one live
// sandbox: the signal the first-run UI polls to detect when the user's
// initial exec has landed.
type FirstActivityView struct {
	Active bool `json:"active"`
}

// firstActivity returns true when the org has real activity: forks_served > 0
// in the instruments snapshot, or at least one live sandbox. It reuses the
// same source calls the instruments and sandboxes handlers use and introduces
// no new data source on the Console struct.
func (c *Console) firstActivity(ctx context.Context, orgID string) (bool, error) {
	snap, err := c.deps.Instruments.Snapshot(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("ctx: %w", err)
	}
	if snap.ForksServed > 0 {
		return true, nil
	}
	boxes, err := c.deps.Sandboxes.List(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("ctx: %w", err)
	}
	return len(boxes) > 0, nil
}

// handleFirstActivity serves GET /console/first-activity. It returns
// {"active": <bool>} with 200 once the org has real activity, and refuses
// an unauthenticated request the same way the instruments handler does.
// Only the org id and the boolean are logged; no secret or token is logged.
func (c *Console) handleFirstActivity(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	active, err := c.firstActivity(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the first-activity signal could not be read"))
		return
	}
	c.deps.Log.Info("console first-activity", "org", orgID, "active", active)
	writeJSON(w, http.StatusOK, FirstActivityView{Active: active})
}
