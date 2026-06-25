package preview

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxAdminBody = 8 << 20 // 8 MiB route-set cap

type adminHandler struct {
	routes *RouteTable
	token  string
	log    *slog.Logger
}

// NewAdminHandler returns the authenticated route-sync endpoint. POST
// /internal/routes with {"routes":[ClaimState...]} replaces the route set
// (RouteTable.Sync). The admin token is a bearer credential, compared in
// constant time and never logged.
func NewAdminHandler(routes *RouteTable, adminToken string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	mux := http.NewServeMux()
	h := &adminHandler{routes: routes, token: adminToken, log: log}
	mux.HandleFunc("POST /internal/routes", h.sync)
	return mux
}

func (h *adminHandler) sync(w http.ResponseWriter, r *http.Request) {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token == "" || !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var payload struct {
		Routes []ClaimState `json:"routes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	h.routes.Sync(payload.Routes)
	h.log.Info("route set synced", "count", len(payload.Routes))
	w.WriteHeader(http.StatusNoContent)
}
