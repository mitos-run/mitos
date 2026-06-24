package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"mitos.run/mitos/internal/vsock"
)

// This file holds the host-side consumer of the Layer 3 guest telemetry bridge
// (issue #164): the /v1/vitals endpoint asks a sandbox's guest agent over gRPC
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

	if err := api.checkSandboxRegistered(sandboxID); err != nil {
		writeErr(w, "sandbox not connected", http.StatusBadGateway)
		return
	}

	// Fetch one vitals sample via gRPC Vitals stream.
	g := newVsockGuestConn(api, sandboxID)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	vs, err := g.Vitals(ctx, time.Second)
	if err != nil {
		writeErr(w, "guest vitals unavailable", http.StatusBadGateway)
		return
	}
	sample, err := vs.Recv()
	vs.Close() //nolint:errcheck // best-effort; we already have the sample or an error
	if err != nil && err != io.EOF {
		writeErr(w, "guest vitals unavailable", http.StatusBadGateway)
		return
	}

	// Fetch process table via gRPC Processes.
	pl, perr := g.Processes(ctx)
	if perr != nil {
		// Non-fatal: return vitals without process table.
		pl = nil
	}

	// Map gRPC GuestVitals to vsock.VitalsResponse for wire compatibility.
	// CpuStealPercent is [0,100]; StealFraction is [0,1].
	//
	// Note: VitalsResponse.SampleWindowMs and ProcessEntry.CPUJiffies are
	// intentionally absent (always 0) on the gRPC vitals path. The guest proto
	// GuestVitals (sandbox.v1) does not carry a sample_window_ms field, and
	// ProcessInfo does not carry cpu_jiffies; these fields were JSON-only in the
	// legacy path. No test or SDK should assert non-zero values for them on the
	// gRPC path.
	v := vsock.VitalsResponse{}
	if sample != nil {
		v.StealFraction = sample.CpuStealPercent / 100.0
		v.MemTotalKB = uint64(sample.MemTotalBytes) / 1024
		v.MemUsedKB = uint64(sample.MemUsedBytes) / 1024
		if v.MemTotalKB > v.MemUsedKB {
			v.MemAvailableKB = v.MemTotalKB - v.MemUsedKB
		}
		v.BalloonReclaimedKB = uint64(sample.MemBalloonBytes) / 1024
		// SampleWindowMs not provided by gRPC GuestVitals; remains 0.
	}
	if pl != nil {
		for _, p := range pl.GetProcesses() {
			v.Processes = append(v.Processes, vsock.ProcessEntry{
				PID:  int(p.Pid),
				Comm: p.Command,
				// CPUJiffies not provided by gRPC ProcessInfo; remains 0.
				State: p.State,
				RSSKB: uint64(p.RssBytes) / 1024,
			})
		}
	}

	api.touch(sandboxID)

	out := LabeledVitals{
		VitalsLabels: api.vitalsLabelsFor(sandboxID),
		Vitals:       v,
	}
	writeJSON(w, out)
}
