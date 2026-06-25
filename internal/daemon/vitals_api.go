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

// NodeVitals is the GET /v1/vitals/node response: one NodeVitalsEntry per sandbox
// this forkd currently serves whose guest answered, so the control plane can
// publish org/pool-labeled guest health metrics WITHOUT holding each sandbox's
// per-sandbox bearer token. It is node-scoped operational data (the same access
// class as /v1/metering, /metrics, and /healthz), NOT per-sandbox traffic, so it
// is served on the operational mux without per-sandbox bearer auth. The Skipped
// count is the number of sandboxes whose guest was unreachable this scrape (a
// degradation signal); no sandbox id or error text is carried for them.
//
// SECRET HYGIENE: this node-scoped endpoint is UNAUTHENTICATED, so it carries ONLY
// the control-plane labels (claim, pool, workspace, namespace; all k8s object
// names), the numeric guest vitals (steal, balloon, used/total memory), and a
// numeric process COUNT. It deliberately does NOT carry the per-process table:
// no process command name, pid, state, rss, argv, env, secret, or token is on the
// wire here. The full per-process table stays behind the per-sandbox bearer
// authenticated /v1/vitals (used by kubectl mitos ps --processes), never on this
// node batch. The control-plane sampler that consumes this reads ONLY the numeric
// fields (including process_count) and the org/pool labels.
type NodeVitals struct {
	Sandboxes []NodeVitalsEntry `json:"sandboxes"`
	Skipped   int               `json:"skipped"`
}

// NodeVitalsEntry is one sandbox's numeric vitals in the node report, plus its
// control-plane labels and forkd SandboxID. The SandboxID is the husk pod name;
// it is NOT a secret (it already flows through /v1/metering) and lets the
// control-plane sampler resolve the trusted mitos.run/org label off the husk pod,
// exactly as the usage scraper does. The sampler uses the SandboxID ONLY to
// resolve org; it never becomes a metric label, so it adds no cardinality.
//
// This struct intentionally carries the NUMERIC vitals inline plus a numeric
// ProcessCount, and NEVER embeds the guest process table (vsock.ProcessEntry). A
// per-process command name, pid, state, or rss CANNOT appear on this unauthenticated
// node-scoped endpoint by construction: there is no field on the wire to hold one.
// ProcessCount is the LENGTH of the guest's process table, the only process signal
// the control-plane sampler needs.
type NodeVitalsEntry struct {
	SandboxID string `json:"sandbox_id"`
	VitalsLabels
	Vitals NodeVitalsNumbers `json:"vitals"`
}

// NodeVitalsNumbers is the numeric-only guest vitals carried on the node-scoped
// batch endpoint: it mirrors the numeric fields of vsock.VitalsResponse but
// replaces the per-process table with a numeric ProcessCount. It exists so the
// unauthenticated node endpoint physically cannot serialize a process command,
// pid, state, or rss: the type has no field for one.
type NodeVitalsNumbers struct {
	StealFraction      float64 `json:"steal_fraction"`
	MemTotalKB         uint64  `json:"mem_total_kb"`
	MemAvailableKB     uint64  `json:"mem_available_kb"`
	MemUsedKB          uint64  `json:"mem_used_kb"`
	BalloonReclaimedKB uint64  `json:"balloon_reclaimed_kb"`
	// ProcessCount is len(guest process table), never a per-process field.
	ProcessCount int `json:"process_count"`
}

// registeredSandboxIDs returns the ids of the sandboxes with a registered vsock
// stream path (the live, guest-reachable set). It is the enumeration the
// node-level vitals handler walks.
func (api *SandboxAPI) registeredSandboxIDs() []string {
	api.mu.RLock()
	defer api.mu.RUnlock()
	ids := make([]string, 0, len(api.streamPaths))
	for id := range api.streamPaths {
		ids = append(ids, id)
	}
	return ids
}

// handleNodeVitals returns one numeric-only NodeVitalsEntry per sandbox this forkd
// serves, for the control-plane vitals sampler (issue #164 Phase 1.a). A sandbox
// whose guest is unreachable is SKIPPED and counted, never failing the whole
// report: one stuck guest must not blind the operator to the healthy fleet. It is
// mounted on the operational mux (no per-sandbox bearer) because it is node-scoped
// operator telemetry, like /v1/metering.
//
// SECRET HYGIENE: this endpoint is UNAUTHENTICATED, so the per-process table is
// stripped at the source here. We read the guest snapshot, take ONLY its numeric
// fields plus len(snapshot.Processes) as ProcessCount, and write the dedicated
// NodeVitalsNumbers struct, which has no field for a process command, pid, state,
// or rss. The full process table is served only by the bearer-authenticated
// /v1/vitals.
func (api *SandboxAPI) handleNodeVitals(w http.ResponseWriter, r *http.Request) {
	out := NodeVitals{}
	for _, id := range api.registeredSandboxIDs() {
		v, err := api.vitalsSnapshot(r.Context(), id)
		if err != nil {
			// Skip-and-count: an unreachable guest degrades to one missing row, never
			// a failed report. No sandbox id or error text is carried for it.
			out.Skipped++
			continue
		}
		out.Sandboxes = append(out.Sandboxes, NodeVitalsEntry{
			SandboxID:    id,
			VitalsLabels: api.vitalsLabelsFor(id),
			Vitals: NodeVitalsNumbers{
				StealFraction:      v.StealFraction,
				MemTotalKB:         v.MemTotalKB,
				MemAvailableKB:     v.MemAvailableKB,
				MemUsedKB:          v.MemUsedKB,
				BalloonReclaimedKB: v.BalloonReclaimedKB,
				// Strip the table: only its LENGTH crosses this unauthenticated wire.
				ProcessCount: len(v.Processes),
			},
		})
	}
	writeJSON(w, out)
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

	v, err := api.vitalsSnapshot(r.Context(), sandboxID)
	if err != nil {
		writeErr(w, "guest vitals unavailable", http.StatusBadGateway)
		return
	}

	api.touch(sandboxID)

	out := LabeledVitals{
		VitalsLabels: api.vitalsLabelsFor(sandboxID),
		Vitals:       v,
	}
	writeJSON(w, out)
}

// vitalsSnapshot asks sandboxID's guest agent for one vitals sample plus the
// process table over gRPC and maps it to the wire-compatible vsock.VitalsResponse.
// It returns an error when the sandbox is not registered or the guest is
// unreachable, so callers (the per-sandbox handler and the node-level batch
// handler) can degrade rather than fabricate a value. The snapshot carries no
// secrets: process entries are program names and resource counters only.
//
// Note: VitalsResponse.SampleWindowMs and ProcessEntry.CPUJiffies are
// intentionally absent (always 0) on the gRPC vitals path. The guest proto
// GuestVitals (sandbox.v1) does not carry a sample_window_ms field, and
// ProcessInfo does not carry cpu_jiffies; these fields were JSON-only in the
// legacy path. No test or SDK should assert non-zero values for them on the
// gRPC path.
func (api *SandboxAPI) vitalsSnapshot(ctx context.Context, sandboxID string) (vsock.VitalsResponse, error) {
	var v vsock.VitalsResponse
	if err := api.checkSandboxRegistered(sandboxID); err != nil {
		return v, err
	}

	g := newVsockGuestConn(api, sandboxID)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	vs, err := g.Vitals(ctx, time.Second)
	if err != nil {
		return v, err
	}
	sample, err := vs.Recv()
	vs.Close() //nolint:errcheck // best-effort; we already have the sample or an error
	if err != nil && err != io.EOF {
		return v, err
	}

	// Fetch process table via gRPC Processes (non-fatal: a missing table yields a
	// zero process_count, never an error).
	pl, perr := g.Processes(ctx)
	if perr != nil {
		pl = nil
	}

	// Map gRPC GuestVitals to vsock.VitalsResponse. CpuStealPercent is [0,100];
	// StealFraction is [0,1].
	if sample != nil {
		v.StealFraction = sample.CpuStealPercent / 100.0
		v.MemTotalKB = uint64(sample.MemTotalBytes) / 1024
		v.MemUsedKB = uint64(sample.MemUsedBytes) / 1024
		if v.MemTotalKB > v.MemUsedKB {
			v.MemAvailableKB = v.MemTotalKB - v.MemUsedKB
		}
		v.BalloonReclaimedKB = uint64(sample.MemBalloonBytes) / 1024
	}
	if pl != nil {
		for _, p := range pl.GetProcesses() {
			v.Processes = append(v.Processes, vsock.ProcessEntry{
				PID:   int(p.Pid),
				Comm:  p.Command,
				State: p.State,
				RSSKB: uint64(p.RssBytes) / 1024,
			})
		}
	}
	return v, nil
}
