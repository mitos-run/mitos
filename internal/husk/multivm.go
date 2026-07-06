package husk

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/metering"
)

// validVMID is the allowlist a vmID must satisfy before it is ever used to
// derive a Firecracker id, workdir, or socket path (deriveVMConfig). It is
// intentionally identical to huskVMIDPattern in cmd/husk-stub (and firecracker's
// internal vmIDPattern): a leading alphanumeric then up to 63 of
// [alphanumeric _ -], which cannot contain "/", "..", or a NUL, so a vmID can
// never traverse out of the pod workdir. defaultVMID satisfies it. Increment 2's
// only live caller passes defaultVMID, but a later increment will thread a vmID
// from the control plane, so the path-traversal gate is closed here now.
var validVMID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// checkVMID rejects a vmID that is not on the validVMID allowlist, so it can
// never be interpolated into a filesystem path unsafely.
func checkVMID(id vmID) error {
	if !validVMID.MatchString(string(id)) {
		return fmt.Errorf("husk: invalid vm id %q: must match %s", string(id), validVMID.String())
	}
	return nil
}

// This file holds the EXPERIMENTAL multi-VM-per-pod execution mode (#764, Layer
// 1 of docs/superpowers/plans/2026-07-06-fork-primitive-multinode.md), gated
// behind Options.MultiVM (default OFF). It multiplexes the husk lifecycle over
// the per-VM instances map so ONE husk pod can host MANY same-tenant VMs, each an
// isolated Firecracker process with its OWN socket, workdir, and lifecycle state
// machine. The single-VM code in stub.go is untouched and remains the only path
// any production caller exercises today (no caller sets MultiVM; the controller
// is not wired to it).
//
// Increment 2 scope (this file): the map-based state-machine multiplexing proven
// with the mock VMM: two distinct vmIDs each Prepare -> Dormant -> Activate ->
// Active independently, Close one without disturbing the other, Metering reports
// every active VM. The per-VM egress tap / DNS proxy programming, the fork ops
// (ForkSnapshot / workspace dehydrate-hydrate) keyed by vmID, and spawning a
// second VM by CoW-restoring from a live parent are DEFERRED to a later increment
// (they need the per-VM networking and real-Firecracker spawn wiring, provable
// only on the KVM firecracker-test suite). This increment proves the state
// machine and multiplexing, not the second real Firecracker process.

// deriveVMConfig returns the firecracker.VMConfig for one VM in a multi-VM pod.
// The default (primary) VM keeps the pod's base config unchanged, so a multi-VM
// pod with a single default VM is configured exactly like a single-VM pod. Every
// OTHER vmID gets its OWN Firecracker identity: a distinct process ID, a
// per-VM workdir nested under the pod workdir, and the API + vsock sockets bound
// inside that per-VM workdir. That per-VM socket/workdir isolation is what lets
// several Firecracker processes coexist in one pod without colliding on the
// single fixed firecracker.sock / vsock.sock the single-VM path uses.
func (s *Stub) deriveVMConfig(id vmID) firecracker.VMConfig {
	cfg := s.cfg
	if id == defaultVMID {
		return cfg
	}
	cfg.ID = s.cfg.ID + "-" + string(id)
	// Nest each non-default VM's workdir under the pod workdir so its Firecracker
	// API socket and the guest vsock UDS (bound relative to WorkDir) never collide
	// with a sibling's. Empty base WorkDir (the unit path, which injects a fake
	// starter that ignores the config) leaves the derived paths empty too.
	if s.cfg.WorkDir != "" {
		cfg.WorkDir = filepath.Join(s.cfg.WorkDir, string(id))
		cfg.SocketPath = filepath.Join(cfg.WorkDir, "firecracker.sock")
	}
	return cfg
}

// prepareInstance brings up a DORMANT Firecracker VMM for one vmID and records it
// on the per-VM instance, allocating the instance on first use. It mirrors the
// single-VM Prepare (state gate, snapshot verify at prepare, per-activation rootfs
// clone) but scoped to ONE entry in the instances map, so preparing a second VM
// never touches the first. Fail closed: any error tears the dormant VMM down and
// leaves the instance out of StateDormant.
func (s *Stub) prepareInstance(ctx context.Context, id vmID) error {
	if err := checkVMID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.instances[id]
	if inst == nil {
		inst = newVMInstance()
		s.instances[id] = inst
	}
	if inst.state != StateNew {
		return fmt.Errorf("husk: prepare vm %q in state %s: already prepared", id, inst.state)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cfg := s.deriveVMConfig(id)
	// Create the per-VM workdir before launching so the Firecracker API socket and
	// vsock UDS have a home. The default VM reuses the pod workdir (already created
	// by cmd/husk-stub), and the unit path has an empty workdir, so only a nested
	// non-default VM needs the mkdir here.
	if cfg.WorkDir != "" && id != defaultVMID {
		if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
			return fmt.Errorf("husk: create per-VM workdir for vm %q: %w", id, err)
		}
	}
	vm, err := s.start(cfg)
	if err != nil {
		return fmt.Errorf("husk: prepare dormant VMM for vm %q: %w", id, err)
	}
	inst.vm = vm

	// Snapshot verify at prepare time (pre-paid, dormant), the same fail-closed
	// gate the single-VM Prepare runs, when the pod was started with the snapshot
	// dir + expected digest. The snapshot is read-only and content-addressed, so
	// verifying once here is equivalent to verifying at activate.
	if s.prepareSnapshotDir != "" && s.prepareExpectedDigest != "" {
		for _, name := range []string{"mem", "vmstate"} {
			f := filepath.Join(s.prepareSnapshotDir, name)
			if err := waitForFile(ctx, f, rootfsTemplateWait); err != nil {
				_ = inst.vm.Close()
				inst.vm = nil
				return fmt.Errorf("husk: snapshot file %s not ready for vm %q: %w", f, id, err)
			}
		}
		if err := s.verify(ActivateRequest{
			SnapshotDir:    s.prepareSnapshotDir,
			ExpectedDigest: s.prepareExpectedDigest,
		}); err != nil {
			_ = inst.vm.Close()
			inst.vm = nil
			return fmt.Errorf("husk: prepare-time snapshot verification failed for vm %q: %w", id, err)
		}
		inst.prepareVerified = true
	}

	// Per-activation rootfs CoW clone, scoped to this VM's derived id so two VMs in
	// the pod never share or overwrite a rootfs clone. Same primitive and dormant
	// pre-paid timing as the single-VM Prepare.
	if s.rootfsTemplatePath != "" && s.rootfsCoWDir != "" {
		clonePath := filepath.Join(s.rootfsCoWDir, cfg.ID, "rootfs.ext4")
		if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
			_ = inst.vm.Close()
			inst.vm = nil
			return fmt.Errorf("husk: create per-activation rootfs dir for vm %q: %w", id, err)
		}
		if err := waitForFile(ctx, s.rootfsTemplatePath, rootfsTemplateWait); err != nil {
			_ = inst.vm.Close()
			inst.vm = nil
			return fmt.Errorf("husk: per-activation rootfs template %s not ready for vm %q: %w", s.rootfsTemplatePath, id, err)
		}
		if err := s.reflink(s.rootfsTemplatePath, clonePath); err != nil {
			_ = inst.vm.Close()
			inst.vm = nil
			return fmt.Errorf("husk: clone per-activation rootfs for vm %q: %w", id, err)
		}
		inst.rootfsClonePath = clonePath
	}

	inst.state = StateDormant
	return nil
}

// activateInstance loads the snapshot into one dormant per-VM instance and waits
// for its guest to answer, scoped to ONE entry in the instances map. It mirrors
// the single-VM Activate state machine (verify -> load paused -> rootfs rebind ->
// resume -> guest ready -> fork-correctness handshake) but drives inst.vm and
// advances inst.state, so activating a second VM neither reads nor mutates the
// first VM's state. Fail closed: any step failing returns OK=false and leaves the
// instance NOT active.
//
// The per-VM in-pod egress filter + DNS proxy (which the single-VM Activate
// programs) are DEFERRED for multi-VM to a later increment: several VMs sharing
// the pod netns need a per-VM tap fan-out that this state-machine increment does
// not build. No production caller runs multi-VM yet, so nothing regresses.
func (s *Stub) activateInstance(ctx context.Context, id vmID, req ActivateRequest) (ActivateResult, error) {
	if err := checkVMID(id); err != nil {
		return ActivateResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.instances[id]
	if inst == nil || inst.state != StateDormant {
		state := StateNew
		if inst != nil {
			state = inst.state
		}
		return ActivateResult{
				OK:            false,
				AlreadyActive: inst != nil && inst.state == StateActive,
				Error:         fmt.Sprintf("activate vm %q in state %s: must be dormant", id, state),
			},
			fmt.Errorf("husk: activate vm %q in state %s: must be dormant", id, state)
	}
	if err := ctx.Err(); err != nil {
		return ActivateResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ActivateResult{OK: false, Error: "activate: empty snapshot dir"},
			fmt.Errorf("husk: activate vm %q: empty snapshot dir", id)
	}

	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")

	start := time.Now()

	// Verify-on-activate gate, with the same prepare-time fast path the single-VM
	// Activate uses: skip the re-hash only when THIS instance verified this exact
	// snapshot during its dormant period.
	if !(inst.prepareVerified && req.SnapshotDir == s.prepareSnapshotDir && req.ExpectedDigest == s.prepareExpectedDigest) {
		if err := s.verify(req); err != nil {
			werr := fmt.Errorf("husk: snapshot verification failed for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// Load PAUSED so the rootfs drive can be rebound before the guest runs, then
	// resume explicitly, exactly as the single-VM path does.
	if err := inst.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, req.NetworkOverrides); err != nil {
		werr := fmt.Errorf("husk: load snapshot from %s for vm %q: %w", req.SnapshotDir, id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	if inst.rootfsClonePath != "" {
		if err := inst.vm.PatchDrive("rootfs", inst.rootfsClonePath); err != nil {
			werr := fmt.Errorf("husk: rebind rootfs drive to per-activation clone for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}
	if err := inst.vm.Resume(); err != nil {
		werr := fmt.Errorf("husk: resume vm %q after rootfs rebind: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	vsockPath := inst.vm.VsockHostPath(firecracker.VsockRelPath)
	if err := s.ready(ctx, vsockPath, s.readyTimeout); err != nil {
		werr := fmt.Errorf("husk: guest not ready after activate for vm %q: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Fork-correctness handshake with a fresh per-VM generation + entropy. Each
	// instance owns its own generation counter, so two VMs never share it. The
	// entropy and secret values are held only in memory and are NEVER logged.
	entropy := make([]byte, entropySize)
	if _, err := rand.Read(entropy); err != nil {
		werr := fmt.Errorf("husk: generate fork entropy for vm %q: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	inst.generation++
	if err := s.notify(vsockPath, inst.generation, entropy, req); err != nil {
		werr := fmt.Errorf("husk: fork-correctness handshake failed for vm %q: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	if s.onActivated != nil {
		if err := s.onActivated(vsockPath, req.Token); err != nil {
			werr := fmt.Errorf("husk: serve sandbox API for activated vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	latency := time.Since(start)
	inst.state = StateActive
	return ActivateResult{
		OK:        true,
		VsockPath: vsockPath,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// closeInstance tears down ONE per-VM instance's VMM and per-activation artifacts
// and returns it to StateNew, leaving every sibling instance untouched. It is the
// per-VM analog of the single-VM Close: closing one VM in the pod must never take
// a sibling down.
func (s *Stub) closeInstance(id vmID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeInstanceLocked(id)
}

// closeInstanceLocked is the s.mu-held body of closeInstance, reused by the pod
// teardown that closes every instance under one lock acquisition.
func (s *Stub) closeInstanceLocked(id vmID) error {
	inst := s.instances[id]
	if inst == nil {
		return nil
	}

	// Best effort: reap this instance's rootfs CoW clone so it does not outlive the
	// VM. Path only is logged on failure; the clone carries no secrets.
	if inst.rootfsClonePath != "" {
		if rmErr := os.Remove(inst.rootfsClonePath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "husk: remove per-activation rootfs clone %s for vm %q: %v\n", inst.rootfsClonePath, id, rmErr)
		}
		inst.rootfsClonePath = ""
	}

	// Tear down any per-VM egress state this instance held. Multi-VM does not
	// program the tap/DNS yet (deferred), so these are nil today; the teardown is
	// here so the field migration is complete and safe when that wiring lands.
	if inst.dnsProxy != nil {
		_ = inst.dnsProxy.Shutdown(context.Background())
		inst.dnsProxy = nil
	}
	if s.netRunner != nil && inst.activeTap != "" {
		_ = teardownEgressFilter(context.Background(), s.netRunner, inst.activeTap)
		inst.activeTap = ""
	}

	var err error
	if inst.vm != nil {
		err = inst.vm.Close()
		inst.vm = nil
	}
	inst.state = StateNew
	return err
}

// closeAllInstances tears down every VM in the pod, the multi-VM analog of the
// single-VM Close called on pod shutdown. It closes each instance under one lock
// acquisition and returns the first teardown error, having still attempted every
// instance so one stuck VM cannot leak the rest.
func (s *Stub) closeAllInstances() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id := range s.instances {
		if err := s.closeInstanceLocked(id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// meteringMulti returns this pod's CoW-aware metering report with ONE sample per
// ACTIVE VM, so a multi-VM pod meters every same-tenant fork it hosts (not just
// one). It mirrors the single-VM Metering per instance: the sample ID is the VM's
// derived config id (the id the controller maps to an org), the memory split is
// the live smaps_rollup of that VM's Firecracker pid, and the disk split follows
// the seed-vs-clone rule. The IO (proc read, file stat) runs OUTSIDE the lock,
// like the single-VM path, so a slow stat never blocks Activate/Close.
//
// SECRET-FREE: the report carries only vm-ids and numeric byte counts.
func (s *Stub) meteringMulti() metering.Report {
	type meterInput struct {
		id    string
		pid   int
		seed  string
		clone string
		tap   string
	}

	s.mu.Lock()
	var inputs []meterInput
	for id, inst := range s.instances {
		if inst.state != StateActive {
			continue
		}
		pid := 0
		if inst.vm != nil {
			pid = inst.vm.PID()
		}
		inputs = append(inputs, meterInput{
			id:    s.deriveVMConfig(id).ID,
			pid:   pid,
			seed:  s.rootfsTemplatePath,
			clone: inst.rootfsClonePath,
			tap:   inst.activeTap,
		})
	}
	s.mu.Unlock()

	if len(inputs) == 0 {
		return metering.Report{}
	}

	samples := make([]metering.Sample, 0, len(inputs))
	for _, in := range inputs {
		memUnique, memShared := s.memStat(in.pid)

		var diskUnique, diskShared int64
		if in.seed != "" {
			seedSize := apparentSize(in.seed)
			diskShared = seedSize
			if in.clone != "" {
				if div := apparentSize(in.clone) - seedSize; div > 0 {
					diskUnique = div
				}
			}
		} else if in.clone != "" {
			diskUnique = apparentSize(in.clone)
		}

		var egress int64
		if in.tap != "" && s.egressBytes != nil {
			egress = s.egressBytes(in.tap)
		}

		samples = append(samples, metering.Sample{
			ID:           in.id,
			MemoryUnique: memUnique,
			MemoryShared: memShared,
			DiskUnique:   diskUnique,
			DiskShared:   diskShared,
			EgressBytes:  egress,
		})
	}
	return metering.Aggregate(samples)
}

// pingInstances checks every prepared VM's Firecracker API socket, the multi-VM
// analog of the single-VM liveness ping the pod's MonitorVMM drives. It returns
// an error as soon as any VM is unresponsive (so a dead sibling still flips the
// pod NotReady) or when no VM is prepared yet. The pings run under the lock's
// snapshot of the handles taken and released quickly, like the single-VM ping.
func (s *Stub) pingInstances() error {
	s.mu.Lock()
	vms := make([]vmm, 0, len(s.instances))
	for _, inst := range s.instances {
		if inst.vm != nil {
			vms = append(vms, inst.vm)
		}
	}
	s.mu.Unlock()

	if len(vms) == 0 {
		return fmt.Errorf("husk: no VMM prepared")
	}
	for _, vm := range vms {
		if err := vm.Ping(); err != nil {
			return err
		}
	}
	return nil
}
