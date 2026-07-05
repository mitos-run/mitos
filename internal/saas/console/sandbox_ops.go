// This file holds the "operate" verbs on a live sandbox: create, fork, exec,
// and the SSE log-stream endpoint (issue #322/#323). They extend the
// SandboxControl / LogStreamer seams declared in seams.go with the same
// org-scoping and RBAC discipline as the existing list/inspect/terminate
// handlers in console.go.
package console

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/placement"
)

// validPlacementNames renders r's available value names as a comma-separated
// list for an LLM-legible 400 remediation (issue #28's rule): "fra, iad"
// rather than a raw JSON dump. Used only by handleCreateSandbox's region
// validation error.
func validPlacementNames(r placement.Registry) string {
	names := make([]string, 0, len(r.Values))
	for _, v := range r.Values {
		if v.Available {
			names = append(names, v.Name)
		}
	}
	return strings.Join(names, ", ")
}

// allowedVCPUs / allowedMemGiB are the console's v1 quota bounds for the
// create-sandbox vcpu/mem selects: conservative, static options (issue #322).
// The SPA renders exactly these; the server re-validates them independently so
// a request that bypasses the SPA cannot smuggle an out-of-band value.
var (
	allowedVCPUs  = map[int32]bool{1: true, 2: true, 4: true}
	allowedMemGiB = map[int32]bool{1: true, 2: true, 4: true, 8: true}
)

const (
	// maxForkCount bounds POST .../fork's count.
	maxForkCount = 16
	// maxExecTimeoutSec bounds POST .../exec's timeout_s: the caller-facing
	// range stays 0..60. An explicit 0 no longer means "no timeout" against
	// the backend: handleExecSandbox substitutes defaultExecTimeoutSec so a
	// caller can never make a shared-BFF-mediated command run unbounded.
	maxExecTimeoutSec = 60
	// defaultExecTimeoutSec is the timeout handleExecSandbox forwards to the
	// SandboxControl seam when the caller's timeout_s is 0. Without this, 0
	// would reach the real backend as "run forever" (mcp.HTTPBackend.Exec's
	// documented behavior, shared with the CLI and not changed here), and an
	// org member's runaway command could hold the console's exec goroutine
	// and its stdout/stderr buffers open indefinitely.
	defaultExecTimeoutSec = 30
	// auditCmdPreviewLen is how much of an exec'd command lands in the audit
	// detail: enough to identify the action, never the full command (which
	// could embed a secret value pasted by the caller) and never env/secrets.
	auditCmdPreviewLen = 80
	// maxExecOutputBytes bounds how much of exec's stdout/stderr the console
	// returns to the caller. A command that floods output over the sandbox's
	// own transport (mcp.HTTPBackend.Exec buffers the full stream in memory,
	// a seam shared with the CLI that this fix deliberately does not touch)
	// must not be allowed to balloon the console's response and hold that
	// memory for the life of the request; each stream is capped
	// independently, not their sum.
	maxExecOutputBytes = 256 * 1024
)

// truncatedOutputMarker is appended (on its own line) to stdout or stderr
// when either was cut off at maxExecOutputBytes, so the caller can tell
// truncated output apart from a command that genuinely produced exactly that
// much text.
const truncatedOutputMarker = "\n[output truncated at 256 KiB]"

// truncateOutput returns s capped at maxExecOutputBytes, with
// truncatedOutputMarker appended when it was cut. The cut point is backed off
// to the nearest rune boundary so a multi-byte UTF-8 rune straddling the
// limit is never split into invalid bytes.
func truncateOutput(s string) string {
	if len(s) <= maxExecOutputBytes {
		return s
	}
	cut := maxExecOutputBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedOutputMarker
}

// createSandboxRequest is the body of POST /console/sandboxes.
type createSandboxRequest struct {
	Template  string `json:"template"`
	VCPUs     int32  `json:"vcpus"`
	MemGiB    int32  `json:"mem_gib"`
	ProjectID string `json:"project_id"`
	// Region is the placement value (issue #712 phase 0) requested for this
	// sandbox's tree root. Empty means the org's home region. Validated
	// against the deployment's placement.Registry in handleCreateSandbox
	// before being forwarded to SandboxControl.Create.
	Region string `json:"region"`
}

// handleCreateSandbox provisions a new sandbox in the caller's org. Authorization
// is the SAME per-project decision Fork/Terminate/Exec use for an existing
// sandbox (canAccessSandbox with PermUseResources), applied here to the
// TARGET project (empty means org-wide): this way a plain org-wide
// resources.use member can create an unassigned sandbox exactly as before,
// AND a caller who only holds a per-project role (e.g. an org-wide Viewer who
// is a project Admin) can create straight into their own project, while a
// resources.use member who is NOT a member of the target project is refused
// (they cannot route around the project-assignment endpoint's stricter
// PermManageProjects gate by tagging project_id on create).
func (c *Console) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req createSandboxRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the sandbox-create body is not valid JSON"))
		return
	}
	if strings.TrimSpace(req.Template) == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("template is required"))
		return
	}
	if !allowedVCPUs[req.VCPUs] {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("vcpus must be one of 1, 2, 4"))
		return
	}
	if !allowedMemGiB[req.MemGiB] {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("mem_gib must be one of 1, 2, 4, 8"))
		return
	}
	if req.Region != "" && !c.deps.Capabilities.Placement.Valid(req.Region) {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("region %q is not a valid %s for this deployment", req.Region, c.deps.Capabilities.Placement.Key)).
			WithRemediation(fmt.Sprintf("Omit region to use the org's home region, or set it to one of: %s.", validPlacementNames(c.deps.Capabilities.Placement))))
		return
	}
	if req.ProjectID != "" {
		found, err := c.validateProjectInOrg(r, orgID, req.ProjectID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).
				WithCause("the project list could not be read"))
			return
		}
		if !found {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the project does not exist or does not belong to this organization"))
			return
		}
	}
	canCreate, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, req.ProjectID, saas.PermUseResources)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the access check could not be completed"))
		return
	}
	if !canCreate {
		apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller's role does not grant resources.use for this project"))
		return
	}
	sb, err := c.deps.Sandboxes.Create(r.Context(), orgID, CreateSandboxRequest{
		Template: req.Template,
		VCPUs:    req.VCPUs,
		MemGiB:   req.MemGiB,
		Region:   req.Region,
	})
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	if req.ProjectID != "" {
		if err := c.deps.ResourceProjects.SetProject(r.Context(), orgID, "sandbox", sb.ID, req.ProjectID); err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).
				WithCause("the sandbox was created but its project assignment could not be stored"))
			return
		}
		sb.ProjectID = req.ProjectID
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "sandbox.create", Target: sb.ID, TargetType: "sandbox", TargetName: sb.Template,
		Detail: fmt.Sprintf("created sandbox %s from template %s", sb.ID, req.Template),
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusCreated, sb)
}

// forkSandboxRequest is the body of POST /console/sandboxes/{id}/fork.
type forkSandboxRequest struct {
	Count int `json:"count"`
}

// handleForkSandbox forks an existing org sandbox into count copies. Follows
// the exact resolve-then-authorize pattern handleTerminateSandbox uses: fetch
// the sandbox (404 if missing/cross-org), resolve its project tag, then check
// per-project PermUseResources (403, not 404, so an authenticated-but-denied
// caller learns the sandbox exists but they lack access).
func (c *Console) handleForkSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, id)
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", sb.ID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
		return
	}
	sb.ProjectID = pid
	canAct, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, sb.ProjectID, saas.PermUseResources)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
		return
	}
	if !canAct {
		apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller's role does not grant access to this sandbox"))
		return
	}
	var req forkSandboxRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the fork body is not valid JSON"))
		return
	}
	if req.Count < 1 || req.Count > maxForkCount {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("count must be between 1 and %d", maxForkCount)))
		return
	}
	ids, err := c.deps.Sandboxes.Fork(r.Context(), orgID, id, req.Count)
	if err != nil {
		if len(ids) == 0 {
			// Total failure: nothing landed, so this is the same honest
			// failure path every other verb uses; there is no survivor list
			// to report.
			c.failSandbox(w, err)
			return
		}
		// Partial failure (issue #716): the underlying seam creates each
		// fork independently (see clustersandbox.Control.Fork's doc) and,
		// on a partial failure, returns however many landed before the
		// failing one alongside the error, rather than rolling the
		// survivors back (Kubernetes has no multi-object transaction, and
		// deleting them back out on error risks compounding one failure
		// into two). Those survivors are REAL, billable sandboxes now;
		// discarding their ids here would silently strand them from the
		// caller's view (they would only ever surface on the next
		// List/ForkTree read, with no link back to this request). Report
		// them via a 207-style body instead of collapsing to a bare error.
		c.audit(r.Context(), AuditEvent{
			OrgID: orgID, ActorID: accountID,
			Action: "sandbox.fork", Target: id, TargetType: "sandbox", TargetName: sb.Template,
			Detail: fmt.Sprintf("forked sandbox %s: %d of %d requested cop%s created before a failure: %s",
				id, len(ids), req.Count, pluralIes(len(ids)), err.Error()),
			At: c.deps.Now(),
		})
		writeJSON(w, http.StatusMultiStatus, map[string]any{
			"org_id": orgID, "source": id, "ids": ids, "error": err.Error(),
		})
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "sandbox.fork", Target: id, TargetType: "sandbox", TargetName: sb.Template,
		Detail: fmt.Sprintf("forked sandbox %s into %d cop%s", id, req.Count, pluralIes(req.Count)),
		At:     c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "source": id, "ids": ids})
}

// pluralIes returns "y" for 1 and "ies" otherwise, so the audit sentence reads
// "1 copy" / "3 copies" instead of always pluralizing.
func pluralIes(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// execSandboxRequest is the body of POST /console/sandboxes/{id}/exec.
type execSandboxRequest struct {
	Cmd      string `json:"cmd"`
	TimeoutS int    `json:"timeout_s"`
}

// execSandboxResponse is the wire shape of a successful exec.
type execSandboxResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// handleExecSandbox runs one command inside an existing org sandbox and
// returns its result. Gated the same way as Fork/Terminate: the sandbox must
// belong to the org (404) and the caller must hold PermUseResources on its
// project (403). The audit detail carries only the first 80 characters of the
// command, NEVER the full command, environment, or any secret value.
//
// Two bounds protect the shared BFF process from a single command, without
// touching internal/mcp (the exec transport shared with the CLI): a
// timeout_s of 0 is NOT forwarded to the backend as "no timeout" (which the
// CLI's convention makes an unbounded run); it is replaced with
// defaultExecTimeoutSec here, so an org member cannot make a command run
// forever. And the returned stdout/stderr are each truncated independently
// at maxExecOutputBytes with a trailing marker line, so a command that
// floods output cannot exhaust this process's memory when the result is
// buffered and serialized.
func (c *Console) handleExecSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, id)
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", sb.ID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
		return
	}
	sb.ProjectID = pid
	canAct, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, sb.ProjectID, saas.PermUseResources)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
		return
	}
	if !canAct {
		apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller's role does not grant access to this sandbox"))
		return
	}
	var req execSandboxRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the exec body is not valid JSON"))
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause("cmd is required"))
		return
	}
	if req.TimeoutS < 0 || req.TimeoutS > maxExecTimeoutSec {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("timeout_s must be between 0 and %d", maxExecTimeoutSec)))
		return
	}
	// A caller's timeout_s of 0 is NOT forwarded as-is: substitute the
	// console's own default so a runaway command against a real backend
	// cannot hold this handler (and the shared BFF process) open forever.
	timeoutSec := req.TimeoutS
	if timeoutSec == 0 {
		timeoutSec = defaultExecTimeoutSec
	}
	res, err := c.deps.Sandboxes.Exec(r.Context(), orgID, id, req.Cmd, timeoutSec)
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID: orgID, ActorID: accountID,
		Action: "sandbox.exec", Target: id, TargetType: "sandbox", TargetName: sb.Template,
		Detail: "executed: " + truncateRunes(req.Cmd, auditCmdPreviewLen),
		At:     c.deps.Now(),
	})
	// Stdout/stderr are capped independently at maxExecOutputBytes before
	// they are serialized into the response, so a high-output command cannot
	// exhaust the console's memory (see maxExecOutputBytes's doc).
	writeJSON(w, http.StatusOK, execSandboxResponse{
		Stdout:   truncateOutput(res.Stdout),
		Stderr:   truncateOutput(res.Stderr),
		ExitCode: res.ExitCode,
	})
}

// truncateRunes returns s truncated to at most n runes (never splitting a
// multi-byte UTF-8 rune), unchanged if it is already that short.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// --- Live log streaming over SSE (GET /console/sandboxes/{id}/logs/stream) ---

// sseHeartbeatInterval is how often handleSandboxLogsStream writes a heartbeat
// comment to keep the connection alive between real log lines. A package var
// (not a const) so a test can shrink it to observe a heartbeat without a real
// 15-second wait.
var sseHeartbeatInterval = 15 * time.Second

// sseLogSink adapts an http.ResponseWriter into a LogSink that writes each
// line as an SSE "data:" event and flushes immediately, so the client sees it
// live. It sets the SSE headers lazily, on the first line, exactly like
// httpLogSink does for the plain /logs route: this way an authorization
// failure that occurs before any line is written can still change the status
// code (WriteHeader has not been called yet).
type sseLogSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
	wrote   bool
}

func (s *sseLogSink) Write(line []byte) error {
	if !s.wrote {
		writeSSEHeaders(s.w)
	}
	s.wrote = true
	if err := writeSSEData(s.w, line); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// writeSSEHeaders sets the response as an SSE stream. Must be called before
// the first byte is written (it implicitly sends the 200 status).
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// writeSSEData writes line as one SSE event. A line carrying embedded
// newlines (a multi-line log entry) is encoded as multiple "data:" fields per
// the SSE spec, so the client's EventSource reassembles it as one event with
// the newlines preserved.
func writeSSEData(w io.Writer, line []byte) error {
	text := strings.TrimSuffix(string(line), "\n")
	for _, part := range strings.Split(text, "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", part); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// handleSandboxLogsStream is the SSE counterpart to handleSandboxLogs: it
// authorizes and streams exactly like the plain route (a cross-org or missing
// sandbox id is 404 with no content, an unsupported real transport is 501),
// then holds the connection open, sending a heartbeat comment every
// sseHeartbeatInterval, until the client disconnects or the server shuts the
// request down (ctx canceled). Any new lines the underlying LogStreamer
// pushes while StreamLogs is still running are forwarded live; when
// StreamLogs returns (the fake and the current in-memory default do so
// immediately after their buffered lines), the heartbeat loop keeps the
// stream alive so a future push-capable transport can reuse this same
// handler unchanged.
func (c *Console) handleSandboxLogsStream(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	flusher, hasFlusher := w.(http.Flusher)
	if !hasFlusher {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("streaming is not supported by this response writer"))
		return
	}
	sink := &sseLogSink{w: w, flusher: flusher}
	id := r.PathValue("id")
	if err := c.deps.Logs.StreamLogs(r.Context(), orgID, id, sink); err != nil {
		if !sink.wrote {
			c.failSandbox(w, err)
		}
		return
	}
	if !sink.wrote {
		writeSSEHeaders(w)
	}
	flusher.Flush()

	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
