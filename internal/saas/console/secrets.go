package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// SecretView is the console's NON-SECRET view of one org secret. It deliberately
// has no value field: the secret value is write-only. After creation the console
// surfaces only this metadata so the value can never be read back through any
// endpoint, mirroring the one-time-raw-key rule for API keys.
//
// Fingerprint is a non-sensitive digest of the value (a truncated SHA-256) so
// the UI can show that a secret changed on rotation without ever exposing the
// value. It is NOT reversible to the value.
type SecretView struct {
	Name        string `json:"name"`
	OrgID       string `json:"org_id"`
	Provider    string `json:"provider"`    // "kube" (default) | "openbao" | ...
	Mode        string `json:"mode"`        // "copy_in" | "external_reference"
	Version     int    `json:"version"`     // bumped on each rotate
	Fingerprint string `json:"fingerprint"` // non-sensitive digest, never the value
}

// SecretStore is the org-scoped secret seam: a provider registry the console
// writes through. Mirrors the paperclip provider model (docs/saas spec §8). It
// is WRITE-ONLY at the BFF: Put sets a value, List/Delete operate on metadata,
// and no method ever returns the value. The REAL providers (kube-namespaced
// Secrets, OpenBao) plug in behind this interface; the in-memory default is the
// tested seam. Every method scopes its effect to the supplied org so cross-org
// isolation holds at the seam, not just the handler.
//
// Resolution of a value for injection into a sandbox is intentionally NOT part
// of this interface: that happens server-side in the controller at sandbox
// materialization, never through the console.
type SecretStore interface {
	// List returns the org's secrets as non-secret metadata.
	List(ctx context.Context, orgID string) ([]SecretView, error)
	// Put creates or rotates the named secret from value and returns its
	// metadata. The value is consumed to compute the fingerprint and is never
	// retained in or returned from the BFF.
	Put(ctx context.Context, orgID, name, value string) (SecretView, error)
	// Delete removes the org's named secret. A secret owned by a different org
	// is reported as ErrNotFound, indistinguishable from a missing one.
	Delete(ctx context.Context, orgID, name string) error
}

// Fingerprint returns the non-sensitive digest the console shows for a secret
// value: a truncated, prefixed SHA-256. Exported so providers compute it the
// same way the in-memory default does.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// handleListSecrets returns the caller org's secrets (metadata only).
func (c *Console) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	secrets, err := c.deps.Secrets.List(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the secret store could not list secrets"))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OrgID   string       `json:"org_id"`
		Secrets []SecretView `json:"secrets"`
	}{OrgID: orgID, Secrets: secrets})
}

// handleCreateSecret creates or rotates a secret. The value is write-only: it is
// never logged and never returned. The response carries only metadata.
func (c *Console) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	actorID, orgID, e, ok := c.authorize(r, saas.PermManageSecrets)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).WithCause("the request body is not valid JSON"))
		return
	}
	if req.Name == "" || req.Value == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).WithCause("name and value are required"))
		return
	}
	view, err := c.deps.Secrets.Put(r.Context(), orgID, req.Name, req.Value)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the secret store could not store the secret"))
		return
	}
	action := "secret.create"
	if view.Version > 1 {
		action = "secret.rotate"
	}
	// The audit detail carries only the non-secret name; never the value.
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: actorID, Action: action,
		Target: view.Name, TargetType: "secret", TargetName: view.Name,
		Detail: "secret " + view.Name, At: c.deps.Now(),
	})
	writeJSON(w, http.StatusCreated, view)
}

// handleDeleteSecret deletes a secret. A cross-org name is reported as 404.
func (c *Console) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	actorID, orgID, e, ok := c.authorize(r, saas.PermManageSecrets)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	name := r.PathValue("name")
	if err := c.deps.Secrets.Delete(r.Context(), orgID, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the secret does not exist or is not in this organization"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the secret could not be deleted"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: actorID, Action: "secret.delete",
		Target: name, TargetType: "secret", TargetName: name,
		Detail: "secret " + name, At: c.deps.Now(),
	})
	w.WriteHeader(http.StatusNoContent)
}
