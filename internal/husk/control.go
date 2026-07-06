// Package husk implements the husk-pod stub: a single-VM process that brings
// up a DORMANT Firecracker VMM at prepare time and ACTIVATES it in place by
// loading a snapshot when an activate request arrives over a control socket.
//
// One husk process owns exactly one VM. The activate path drives a VMM and is
// security sensitive: it FAILS CLOSED. Any snapshot-load or guest-readiness
// failure leaves the stub NOT active and reports an error; it never reports a
// usable VM it could not verify.
package husk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/vsock"
	"mitos.run/mitos/internal/workspace"
)

// ActivateRequest is the control message asking the dormant VMM to load a
// snapshot in place. SnapshotDir is the template snapshot directory; the stub
// reads the mem and vmstate files from it using the same layout the fork engine
// writes (SnapshotDir/mem and SnapshotDir/vmstate). NetworkOverrides remap the
// snapshot's baked placeholder NIC to this husk's tap, exactly as the engine
// fork path passes them.
//
// Env and Secrets are the claim-time guest configuration delivered after the
// restore handshake, mirroring the daemon's deliverConfig. Network and Volumes
// are the per-fork guest network and volume-mount table threaded into the
// NotifyForked handshake, for parity with the engine fork path.
//
// Secret VALUES are never logged anywhere in the control path: the codec moves
// them, but no log or error message ever prints them. In this PR the secrets
// ride the local control socket only; the real claim-time secret source is the
// controller, delivered to the husk pod's stub by the future controller-
// migration PR.
//
// Token is the per-sandbox bearer token the controller mints for this claim. The
// stub registers it as the gate on the in-pod sandbox HTTP API (exec/files) it
// serves after a successful activate, so only a caller presenting this token can
// reach the activated VM. It is a SECRET: it rides the mTLS control channel and
// is NEVER logged.
type ActivateRequest struct {
	SnapshotDir string `json:"snapshot_dir"`
	// ExpectedDigest is the template's recorded CAS manifest digest (a content
	// address, NOT a secret). The controller passes the digest forkd reported via
	// GetCapacity (the NodeRegistry TemplateDigests), and the stub re-verifies the
	// on-disk snapshot against it BEFORE loading: the mounted manifest must hash
	// to this digest and the loaded mem+vmstate must re-hash to the manifest, so a
	// snapshot tampered on the node disk is refused (fail-closed, the husk mirror
	// of forkd's verify-on-load gate, issues #9 and #32). It is safe to log but is
	// kept out of noisy logging. An empty digest is refused unless the stub runs
	// with the development --allow-unverified-snapshots escape hatch.
	ExpectedDigest   string                        `json:"expected_digest,omitempty"`
	NetworkOverrides []firecracker.NetworkOverride `json:"network_overrides,omitempty"`
	Env              map[string]string             `json:"env,omitempty"`
	Secrets          map[string]string             `json:"secrets,omitempty"`
	Network          *vsock.NotifyForkedNetwork    `json:"network,omitempty"`
	Volumes          []vsock.VolumeMountEntry      `json:"volumes,omitempty"`
	Token            string                        `json:"token,omitempty"`
	// Egress is the template's egress policy ("deny" or "allow") the stub uses
	// to render the in-pod nftables chain's final verdict. Empty defaults to the
	// fail-closed "deny". It is config, not a secret, and is safe to log.
	Egress string `json:"egress,omitempty"`
	// Allow is the template's raw egress allowlist (host:port entries, IP or
	// name) the stub splits into static IP:port chain accepts (SplitAllowList)
	// and name entries the in-pod DNS proxy enforces (ParseNameAllowList). It is
	// config, not a secret, and is safe to log.
	Allow []string `json:"allow,omitempty"`
	// BlockNetwork drops ALL egress for the sandbox (Modal block_network=True),
	// overriding Egress and the allowlists. Config, not a secret.
	BlockNetwork bool `json:"block_network,omitempty"`
	// AllowCIDRs is the egress CIDR allowlist (Modal outbound_cidr_allowlist):
	// destination IPs inside these blocks are accepted. v4 or v6. Config.
	AllowCIDRs []string `json:"allow_cidrs,omitempty"`
	// Inbound governs unsolicited inbound to the guest: "deny" (the secure
	// default, deny-by-default) or "allow". Empty means deny. Config.
	Inbound string `json:"inbound,omitempty"`
	// InboundCIDRs narrows an Inbound=allow to source CIDRs (Modal
	// inbound_cidr_allowlist). Config.
	InboundCIDRs []string `json:"inbound_cidrs,omitempty"`
	// VMID names WHICH same-tenant VM in the pod this activate addresses, the
	// explicit selector for the experimental multi-VM-per-pod mode (#764). It is
	// honored ONLY when the stub runs with --multi-vm; on the default single-VM
	// path it is ignored entirely, so the field changes no shipped behavior. Empty
	// (every caller today, and omitempty keeps it off the wire) defaults to the
	// pod's single implicit VM, so an existing activate stays byte-for-byte
	// compatible. It is a node-local identifier, not a secret.
	VMID string `json:"vm_id,omitempty"`
}

// ActivateResult is the control reply. OK is true only when the snapshot loaded
// AND the guest agent answered over vsock. VsockPath is the host UDS path of the
// activated guest agent (only meaningful when OK). LatencyMs is the wall time
// from the activate call to guest readiness. Error carries actionable
// remediation text when OK is false; it never carries secrets.
type ActivateResult struct {
	OK        bool    `json:"ok"`
	VsockPath string  `json:"vsock_path,omitempty"`
	LatencyMs float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
	// AlreadyActive is set when Activate was refused because the stub is ALREADY
	// in the active state (a prior Activate succeeded). It is not OK, but it tells
	// an idempotent caller that the VM is in fact activated, so a re-drive that
	// lost its ack (or whose post-activate bookkeeping failed) can ADOPT the VM
	// instead of retrying forever (issue #183).
	AlreadyActive bool `json:"already_active,omitempty"`
}

// WriteRequest writes an ActivateRequest as one line of JSON followed by a
// newline. The line-delimited framing lets a peer ReadRequest one message
// without buffering the whole stream.
func WriteRequest(w io.Writer, req ActivateRequest) error {
	return writeLine(w, req)
}

// ReadRequest reads one line-delimited ActivateRequest from r.
func ReadRequest(r io.Reader) (ActivateRequest, error) {
	var req ActivateRequest
	err := readLine(r, &req)
	return req, err
}

// WriteResult writes an ActivateResult as one line of JSON followed by a
// newline.
func WriteResult(w io.Writer, res ActivateResult) error {
	return writeLine(w, res)
}

// ReadResult reads one line-delimited ActivateResult from r.
func ReadResult(r io.Reader) (ActivateResult, error) {
	var res ActivateResult
	err := readLine(r, &res)
	return res, err
}

// ControlOp discriminates the control message that follows it on the wire, so
// one mTLS control channel can serve activate, fork-snapshot, and
// remove-fork-snapshot. The op line is written BEFORE the request line. The
// zero value (an absent op line) is OpActivate, so the existing activate client
// that writes an ActivateRequest directly stays wire-compatible.
type ControlOp struct {
	Op string `json:"op"`
}

const (
	// OpActivate loads a snapshot into a dormant VMM (the default).
	OpActivate = "activate"
	// OpForkSnapshot snapshots the running VM this stub holds.
	OpForkSnapshot = "fork-snapshot"
	// OpRemoveForkSnapshot deletes a previously created fork snapshot dir.
	OpRemoveForkSnapshot = "remove-fork-snapshot"
	// OpDehydrateWorkspace captures the active VM's /workspace into the node CAS
	// and returns the content manifest digest.
	OpDehydrateWorkspace = "dehydrate-workspace"
	// OpHydrateWorkspace restores a node-CAS manifest into the active VM's
	// /workspace.
	OpHydrateWorkspace = "hydrate-workspace"
	// OpSpawnVM brings up an ADDITIONAL same-tenant VM (a new vmID) in a running
	// husk pod using the experimental multi-VM engine. It is honored ONLY when the
	// stub runs with --multi-vm; a single-VM stub fails it closed.
	OpSpawnVM = "spawn-vm"
)

// WriteControlOp writes the op envelope line that precedes a request.
func WriteControlOp(w io.Writer, op string) error {
	return writeLine(w, ControlOp{Op: op})
}

// ReadControlOp reads the op envelope line. An absent op defaults to OpActivate
// so a caller can pair it with the request read on the same connection.
func ReadControlOp(r *bufio.Reader) (string, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("read control op: %w", err)
	}
	var op ControlOp
	if err := json.Unmarshal(line, &op); err != nil {
		return "", fmt.Errorf("decode control op: %w", err)
	}
	if op.Op == "" {
		op.Op = OpActivate
	}
	return op.Op, nil
}

// readActivateRequest decodes one ActivateRequest line from a shared reader.
func readActivateRequest(r *bufio.Reader) (ActivateRequest, error) {
	var req ActivateRequest
	err := readLineReader(r, &req)
	return req, err
}

func readForkSnapshotRequest(r *bufio.Reader) (ForkSnapshotRequest, error) {
	var req ForkSnapshotRequest
	err := readLineReader(r, &req)
	return req, err
}

func readRemoveForkSnapshotRequest(r *bufio.Reader) (RemoveForkSnapshotRequest, error) {
	var req RemoveForkSnapshotRequest
	err := readLineReader(r, &req)
	return req, err
}

// ReadActivateRequestReader decodes one ActivateRequest from a shared reader.
// Exported for the controller-side client and tests that pipeline op + request
// on one connection.
func ReadActivateRequestReader(r *bufio.Reader) (ActivateRequest, error) {
	return readActivateRequest(r)
}

// ReadForkSnapshotRequestReader decodes one ForkSnapshotRequest from a shared reader.
func ReadForkSnapshotRequestReader(r *bufio.Reader) (ForkSnapshotRequest, error) {
	return readForkSnapshotRequest(r)
}

// ReadSpawnVMRequestReader decodes one SpawnVMRequest from a shared reader.
// Exported for the controller-side client and tests that pipeline op + request
// on one connection.
func ReadSpawnVMRequestReader(r *bufio.Reader) (SpawnVMRequest, error) {
	return readSpawnVMRequest(r)
}

// readLineReader decodes one newline-delimited JSON value from a shared reader,
// so multiple reads on one connection (op then request) do not each buffer the
// whole stream the way a per-call bufio.Scanner would.
func readLineReader(r *bufio.Reader, v interface{}) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read control message: %w", err)
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("decode control message: %w", err)
	}
	return nil
}

// ForkSnapshotRequest asks a husk stub holding a RUNNING (active) VM to snapshot
// that VM in place: pause it, write a Full Firecracker snapshot to SnapshotDir
// (mem + vmstate, the same layout the fork engine and the activate path use),
// freeze the source rootfs to SnapshotDir/rootfs.ext4, then resume it. The result
// snapshot is the restore image the controller activates N independent CHILD husk
// pods from, so a husk sandbox can be live-forked even though forkd's engine does
// not own its VM.
//
// ForkID is the node-local fork identifier the controller mints (the SandboxFork
// child group); it is a content/address-like value, never a secret, and is the
// directory leaf under the node forks dir. SnapshotDir is the in-pod path the
// node forks dir is mounted at for THIS fork; the stub writes SnapshotDir/mem,
// SnapshotDir/vmstate, and SnapshotDir/rootfs.ext4.
//
// PauseSource is a compatibility field. The pause is always internal to the
// checkpoint and the source is always resumed afterward, so PauseSource no longer
// leaves the source stopped (leaving it paused was the v1.24.1 production bug).
type ForkSnapshotRequest struct {
	ForkID      string `json:"fork_id"`
	SnapshotDir string `json:"snapshot_dir"`
	PauseSource bool   `json:"pause_source,omitempty"`
}

// ForkSnapshotResult is the control reply for a ForkSnapshot op. OK is true only
// when the VM paused, the snapshot was written, the source rootfs was frozen IF a
// per-activation rootfs clone exists, and the VM resumed. The freeze is
// CONDITIONAL: ForkSnapshot skips it when the stub has no on-disk rootfs clone
// (the mock/CI paths) and still returns OK=true, so OK=true means paused +
// snapshot written + (rootfs frozen only when a clone path exists) + resumed.
// SnapshotDir echoes where the snapshot was written (it carries no secret). Error
// carries actionable remediation text when OK is false; it never carries secrets.
type ForkSnapshotResult struct {
	OK          bool    `json:"ok"`
	SnapshotDir string  `json:"snapshot_dir,omitempty"`
	LatencyMs   float64 `json:"latency_ms"`
	Error       string  `json:"error,omitempty"`
}

// RemoveForkSnapshotRequest asks the source stub to delete a fork snapshot dir it
// previously created. The controller sends it when the SandboxFork is deleted so
// the node-local fork snapshot does not outlive its owner.
type RemoveForkSnapshotRequest struct {
	ForkID      string `json:"fork_id"`
	SnapshotDir string `json:"snapshot_dir"`
}

// WriteForkSnapshotRequest writes a ForkSnapshotRequest as one JSON line.
func WriteForkSnapshotRequest(w io.Writer, req ForkSnapshotRequest) error {
	return writeLine(w, req)
}

// ReadForkSnapshotRequest reads one line-delimited ForkSnapshotRequest.
func ReadForkSnapshotRequest(r io.Reader) (ForkSnapshotRequest, error) {
	var req ForkSnapshotRequest
	err := readLine(r, &req)
	return req, err
}

// WriteForkSnapshotResult writes a ForkSnapshotResult as one JSON line.
func WriteForkSnapshotResult(w io.Writer, res ForkSnapshotResult) error {
	return writeLine(w, res)
}

// ReadForkSnapshotResult reads one line-delimited ForkSnapshotResult.
func ReadForkSnapshotResult(r io.Reader) (ForkSnapshotResult, error) {
	var res ForkSnapshotResult
	err := readLine(r, &res)
	return res, err
}

// WriteRemoveForkSnapshotRequest writes a RemoveForkSnapshotRequest as one JSON line.
func WriteRemoveForkSnapshotRequest(w io.Writer, req RemoveForkSnapshotRequest) error {
	return writeLine(w, req)
}

// ReadRemoveForkSnapshotRequest reads one line-delimited RemoveForkSnapshotRequest.
func ReadRemoveForkSnapshotRequest(r io.Reader) (RemoveForkSnapshotRequest, error) {
	var req RemoveForkSnapshotRequest
	err := readLine(r, &req)
	return req, err
}

// SpawnVMRequest asks a RUNNING husk pod to bring up an ADDITIONAL same-tenant VM
// (a new vmID) inside the same pod, using the experimental multi-VM engine
// (Options.MultiVM, #764). The stub prepares a dormant per-VM Firecracker for
// VMID then activates it from Activate, so a spawn-vm is a prepare + activate of
// one more VM in a pod that is already serving.
//
// VMID names the NEW VM to spawn. It is the authoritative routing selector: the
// stub validates it with checkVMID and derives this VM's per-VM socket, workdir,
// tap, and /30 from it, so it must be a safe node-local identifier (never a path
// traversal). Activate carries the SAME activation inputs a plain activate takes
// (snapshot dir/source and expected digest, per-fork network, egress policy and
// allowlists, inbound policy, secrets, env, token); its own VMID field is not the
// selector and is ignored, the top-level VMID governs. Activate.Secrets and
// Activate.Token are SECRETS: they ride the mTLS control channel and are NEVER
// logged.
type SpawnVMRequest struct {
	VMID     string          `json:"vm_id"`
	Activate ActivateRequest `json:"activate"`
}

// SpawnVMResult is the control reply for a spawn-vm op. It mirrors the fields
// activateInstance returns for the new VM. OK is true only when the additional VM
// prepared, loaded its snapshot, and its guest agent answered over vsock.
// VsockPath is the host UDS path of the newly spawned guest agent (only meaningful
// when OK). VMID echoes which VM this result is for. LatencyMs is the activate
// wall time. AlreadyActive is set when the spawn was refused because that vmID is
// already active. Error carries actionable remediation text when OK is false; it
// never carries secrets.
type SpawnVMResult struct {
	OK            bool    `json:"ok"`
	VMID          string  `json:"vm_id,omitempty"`
	VsockPath     string  `json:"vsock_path,omitempty"`
	LatencyMs     float64 `json:"latency_ms"`
	Error         string  `json:"error,omitempty"`
	AlreadyActive bool    `json:"already_active,omitempty"`
}

func readSpawnVMRequest(r *bufio.Reader) (SpawnVMRequest, error) {
	var req SpawnVMRequest
	err := readLineReader(r, &req)
	return req, err
}

// WriteSpawnVMRequest writes a SpawnVMRequest as one JSON line.
func WriteSpawnVMRequest(w io.Writer, req SpawnVMRequest) error {
	return writeLine(w, req)
}

// ReadSpawnVMRequest reads one line-delimited SpawnVMRequest.
func ReadSpawnVMRequest(r io.Reader) (SpawnVMRequest, error) {
	var req SpawnVMRequest
	err := readLine(r, &req)
	return req, err
}

// WriteSpawnVMResult writes a SpawnVMResult as one JSON line.
func WriteSpawnVMResult(w io.Writer, res SpawnVMResult) error {
	return writeLine(w, res)
}

// ReadSpawnVMResult reads one line-delimited SpawnVMResult.
func ReadSpawnVMResult(r io.Reader) (SpawnVMResult, error) {
	var res SpawnVMResult
	err := readLine(r, &res)
	return res, err
}

// DehydrateWorkspaceRequest asks a husk stub holding a RUNNING (active) VM to
// capture its guest /workspace into the node content-addressed store and return
// the resulting manifest digest. The capture runs the same KVM-proven bulk tar
// round trip the controller's in-process path used (vsock TarDir over
// /workspace, then internal/workspace.Dehydrate into the node CAS), so the
// returned digest is identical to what an in-controller capture would produce.
//
// ExcludePaths strips conventional secret/credential paths from the captured
// tree (defense in depth for the no-secrets-in-revisions rule); CapturePaths
// narrows the capture to the listed /workspace subtrees (nil captures the whole
// workspace). Neither carries a secret VALUE: they are path lists, safe to move
// on the wire. The request itself carries no secret.
//
// ParentManifestDigest, when set, asks the stub to ALSO compute the content-hash
// diff of the just-captured revision against that parent manifest and return it
// in the result. The diff is computed from the two MANIFESTS (path -> chunk-digest
// lists) in the node CAS, never the chunk bytes, reusing the same
// internal/workspace.DiffManifests helper the in-controller path used. The
// controller is not on the node and cannot read either manifest, so the diff must
// run here where the node CAS lives. It is a content address, NOT a secret. Empty
// skips the diff (a {diff: false} terminate, or the first revision in a workspace
// with no parent head).
type DehydrateWorkspaceRequest struct {
	ExcludePaths         []string `json:"exclude_paths,omitempty"`
	CapturePaths         []string `json:"capture_paths,omitempty"`
	ParentManifestDigest string   `json:"parent_manifest_digest,omitempty"`
}

// DehydrateWorkspaceResult is the control reply for a dehydrate-workspace op. OK
// is true only when the guest workspace was tarred, captured to the node CAS,
// and a valid content manifest digest was produced. ManifestDigest is that
// content address (NOT a secret); the controller records it as the new
// WorkspaceRevision's ContentManifest. Error carries actionable remediation text
// when OK is false; it never carries secrets or content bytes.
//
// Diff, when non-nil, is the content-hash diff of the captured revision against
// the request's ParentManifestDigest (the added/removed/modified workspace-
// relative file names). It is only set when ParentManifestDigest was supplied; a
// {diff: false} terminate leaves it nil. The path names are content identifiers,
// not secrets, and no chunk bytes ride the result.
type DehydrateWorkspaceResult struct {
	OK             bool            `json:"ok"`
	ManifestDigest string          `json:"manifest_digest,omitempty"`
	Diff           *workspace.Diff `json:"diff,omitempty"`
	LatencyMs      float64         `json:"latency_ms"`
	Error          string          `json:"error,omitempty"`
}

// HydrateWorkspaceRequest asks a husk stub holding a RUNNING (active) VM to
// restore a node-CAS manifest into its guest /workspace (the inverse of
// DehydrateWorkspace), running internal/workspace.Hydrate: it materializes the
// manifest's files from the node CAS and sends them to the guest over vsock
// (UntarDir, which sanitizes every member against traversal). ManifestDigest is
// a content address, NOT a secret. The request carries no secret.
type HydrateWorkspaceRequest struct {
	ManifestDigest string `json:"manifest_digest"`
}

// HydrateWorkspaceResult is the control reply for a hydrate-workspace op. OK is
// true only when the manifest was read from the node CAS and untarred into the
// guest workspace. Error carries actionable remediation text when OK is false;
// it never carries secrets or content bytes.
type HydrateWorkspaceResult struct {
	OK        bool    `json:"ok"`
	LatencyMs float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

func readDehydrateWorkspaceRequest(r *bufio.Reader) (DehydrateWorkspaceRequest, error) {
	var req DehydrateWorkspaceRequest
	err := readLineReader(r, &req)
	return req, err
}

func readHydrateWorkspaceRequest(r *bufio.Reader) (HydrateWorkspaceRequest, error) {
	var req HydrateWorkspaceRequest
	err := readLineReader(r, &req)
	return req, err
}

// WriteDehydrateWorkspaceRequest writes a DehydrateWorkspaceRequest as one JSON line.
func WriteDehydrateWorkspaceRequest(w io.Writer, req DehydrateWorkspaceRequest) error {
	return writeLine(w, req)
}

// ReadDehydrateWorkspaceResult reads one line-delimited DehydrateWorkspaceResult.
func ReadDehydrateWorkspaceResult(r io.Reader) (DehydrateWorkspaceResult, error) {
	var res DehydrateWorkspaceResult
	err := readLine(r, &res)
	return res, err
}

// WriteDehydrateWorkspaceResult writes a DehydrateWorkspaceResult as one JSON line.
func WriteDehydrateWorkspaceResult(w io.Writer, res DehydrateWorkspaceResult) error {
	return writeLine(w, res)
}

// WriteHydrateWorkspaceRequest writes a HydrateWorkspaceRequest as one JSON line.
func WriteHydrateWorkspaceRequest(w io.Writer, req HydrateWorkspaceRequest) error {
	return writeLine(w, req)
}

// ReadHydrateWorkspaceResult reads one line-delimited HydrateWorkspaceResult.
func ReadHydrateWorkspaceResult(r io.Reader) (HydrateWorkspaceResult, error) {
	var res HydrateWorkspaceResult
	err := readLine(r, &res)
	return res, err
}

// WriteHydrateWorkspaceResult writes a HydrateWorkspaceResult as one JSON line.
func WriteHydrateWorkspaceResult(w io.Writer, res HydrateWorkspaceResult) error {
	return writeLine(w, res)
}

func writeLine(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write control message: %w", err)
	}
	return nil
}

func readLine(r io.Reader, v interface{}) error {
	scanner := bufio.NewScanner(r)
	// Control messages are tiny, but a snapshot dir plus override list is
	// unbounded in principle; allow a generous line so a long request is not
	// silently truncated.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read control message: %w", err)
		}
		return io.EOF
	}
	if err := json.Unmarshal(scanner.Bytes(), v); err != nil {
		return fmt.Errorf("decode control message: %w", err)
	}
	return nil
}
