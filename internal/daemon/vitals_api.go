package daemon

import (
	"encoding/json"
	"net/http"

	"mitos.run/mitos/internal/vsock"
)

// This file holds the host-side consumer of the Layer 3 guest telemetry bridge
// (issue #164): the /v1/vitals endpoint asks a sandbox's guest agent over vsock
// for a one-shot vitals snapshot (CPU steal, memory vs balloon, in-guest process
// table) and returns it LABELED with the claim/pool/workspace identity the host
// knows. It is what `kubectl mitos ps`/top consume to show REAL guest
// processes and vitals; without a reachable guest they fall back to the object
// listing. The endpoint is per-sandbox traffic (it returns one sandbox's process
// table), so it is mounted under the per-sandbox bearer middleware, unlike the
// node-level /v1/metering report.

// VitalsLabels is the control-plane identity the host attaches to a sandbox's
// guest telemetry: the claim, pool, and workspace names. They are k8s object
// names, never secrets. Any field may be empty when the host does not know it
// (e.g. a poolless direct fork); an empty field is reported as empty, never
// guessed.
type VitalsLabels struct {
	Claim     string `json:"claim,omitempty"`
	Pool      string `json:"pool,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// LabeledVitals is the /v1/vitals response: the guest snapshot plus the host's
// claim/pool/workspace labels. The labels let an operator (or `kubectl mitos
// ps`) attribute the in-guest processes and steal to a specific claim.
type LabeledVitals struct {
	VitalsLabels
	Vitals vsock.VitalsResponse `json:"vitals"`
}

// SetVitalsLabels records the claim/pool/workspace identity for sandboxID so its
// /v1/vitals snapshot is labeled. forkd calls it on the Fork path with the same
// identity the OTel spans carry. Calling it again replaces the labels. The
// labels are object names, never secrets.
func (api *SandboxAPI) SetVitalsLabels(sandboxID string, labels VitalsLabels) {
	api.mu.Lock()
	api.vitalsLabels[sandboxID] = labels
	api.mu.Unlock()
}

// vitalsLabelsFor returns the recorded labels for sandboxID (zero value when
// none were recorded).
func (api *SandboxAPI) vitalsLabelsFor(sandboxID string) VitalsLabels {
	api.mu.RLock()
	l := api.vitalsLabels[sandboxID]
	api.mu.RUnlock()
	return l
}

// handleVitals asks the sandbox's guest agent for a telemetry snapshot and
// returns it labeled. A guest that is unreachable or errors yields a 502 so the
// caller (kubectl mitos ps/top) can fall back to the object listing rather
// than render a fabricated value. The snapshot carries no secrets: process
// entries are program names and resource counters only.
func (api *SandboxAPI) handleVitals(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sandbox string `json:"sandbox"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", http.StatusBadRequest)
		return
	}
	sandboxID := api.resolveSandboxID(req.Sandbox)

	agent, err := api.getAgent(sandboxID)
	if err != nil {
		writeErr(w, "sandbox not connected", http.StatusBadGateway)
		return
	}
	v, err := agent.Vitals()
	if err != nil {
		writeErr(w, "guest vitals unavailable", http.StatusBadGateway)
		return
	}
	api.touch(sandboxID)

	out := LabeledVitals{
		VitalsLabels: api.vitalsLabelsFor(sandboxID),
		Vitals:       *v,
	}
	writeJSON(w, out)
}
