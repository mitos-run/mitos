package runservice

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxRequestBytes caps the run request body (a small JSON of src plus secrets).
const maxRequestBytes = 256 << 10

// IdentityResolver maps an authenticated request plus the source repo to the
// tenant namespace and the instance label. The production resolver reads the
// signed-in user (the OIDC session), maps to the org namespace (#410), and builds
// a per-user-per-app label; an unauthenticated request fails so the funnel routes
// it to signup first.
type IdentityResolver interface {
	Resolve(r *http.Request, src string) (Identity, error)
}

// Handler serves the run endpoints.
type Handler struct {
	svc      *Service
	identity IdentityResolver
}

// NewHandler wires a Service and an identity resolver into HTTP handlers.
func NewHandler(svc *Service, id IdentityResolver) *Handler {
	return &Handler{svc: svc, identity: id}
}

// Routes registers GET /run/describe and POST /run on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /run/describe", h.describe)
	mux.HandleFunc("POST /run", h.run)
}

// describe returns the consent contract for a source repo (no auth required: it is
// the public preview the badge links to).
func (h *Handler) describe(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	if src == "" {
		writeError(w, http.StatusBadRequest, "src query parameter is required")
		return
	}
	d, err := h.svc.Describe(r.Context(), src)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type runRequest struct {
	Src     string            `json:"src"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// run provisions an instance for the signed-in user.
func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Src == "" {
		writeError(w, http.StatusBadRequest, "src is required")
		return
	}
	id, err := h.identity.Resolve(r, req.Src)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "sign in to run this: "+err.Error())
		return
	}
	res, err := h.svc.Run(r.Context(), req.Src, id, req.Secrets)
	if err != nil {
		// Provisioning errors are actionable (for example a missing required
		// secret) and never contain secret values.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
