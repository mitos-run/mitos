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

// instanceFor returns the per-VM instance for id. It holds s.mu ONLY for the map
// lookup (and, when create is true, the first-use insert), then releases it, so
// the caller can take the returned instance's OWN lock for the blocking per-VM
// work without holding the shared Stub lock. Instances are never removed from the
// map (Close resets an entry to StateNew rather than deleting it), so the returned
// pointer stays valid after s.mu is released. When create is false and no entry
// exists it returns nil. Map access is ALWAYS under s.mu.
func (s *Stub) instanceFor(id vmID, create bool) *vmInstance {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.instances[id]
	if inst == nil && create {
		inst = newVMInstance()
		s.instances[id] = inst
	}
	return inst
}

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
	// Short critical section on s.mu (inside instanceFor): allocate/insert this
	// vmID's map entry, then release s.mu. The blocking VMM start + snapshot verify
	// + rootfs clone below run under THIS instance's own lock, so preparing a second
	// VM never waits behind the first's blocking I/O.
	inst := s.instanceFor(id, true)
	inst.mu.Lock()
	defer inst.mu.Unlock()

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
		return ActivateResult{OK: false, Error: err.Error()}, err
	}
	// Short critical section on s.mu (inside instanceFor): look up this vmID's map
	// entry, then release s.mu. Activate does NOT create an instance, so a missing
	// entry reads as StateNew. The blocking snapshot load + guest-ready wait + fork
	// handshake below run under THIS instance's own lock, so activating a second VM
	// never waits behind the first's blocking I/O.
	inst := s.instanceFor(id, false)
	if inst == nil {
		return ActivateResult{
				OK:            false,
				AlreadyActive: false,
				Error:         fmt.Sprintf("activate vm %q in state %s: must be dormant", id, StateNew),
			},
			fmt.Errorf("husk: activate vm %q in state %s: must be dormant", id, StateNew)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.state != StateDormant {
		return ActivateResult{
				OK:            false,
				AlreadyActive: inst.state == StateActive,
				Error:         fmt.Sprintf("activate vm %q in state %s: must be dormant", id, inst.state),
			},
			fmt.Errorf("husk: activate vm %q in state %s: must be dormant", id, inst.state)
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
	// Short critical section on s.mu (inside instanceFor): look up the entry, then
	// release s.mu so the blocking VMM Close below runs under this instance's own
	// lock and never blocks a sibling's lifecycle. The entry stays in the map
	// (reset to StateNew), matching the single-VM Close which keeps the Stub alive.
	inst := s.instanceFor(id, false)
	if inst == nil {
		return nil
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return s.closeInstanceBody(id, inst)
}

// closeInstanceBody tears one instance down. The caller holds inst.mu (never
// s.mu across the blocking VMM Close). It reaps the per-activation artifacts and
// closes the VMM, returning the instance to StateNew.
func (s *Stub) closeInstanceBody(id vmID, inst *vmInstance) error {
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
	// Snapshot the (id, instance) pairs under s.mu, then release it so each VMM
	// Close runs under only that instance's own lock. Holding s.mu across the
	// blocking closes would serialize teardown and block every other map operation.
	type entry struct {
		id   vmID
		inst *vmInstance
	}
	s.mu.Lock()
	entries := make([]entry, 0, len(s.instances))
	for id, inst := range s.instances {
		entries = append(entries, entry{id: id, inst: inst})
	}
	s.mu.Unlock()

	var firstErr error
	for _, e := range entries {
		e.inst.mu.Lock()
		err := s.closeInstanceBody(e.id, e.inst)
		e.inst.mu.Unlock()
		if err != nil && firstErr == nil {
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

	// Snapshot the (id, instance) pairs under s.mu, then release it. Each
	// instance's fields are read under its OWN lock, briefly, so metering never
	// holds s.mu while an instance lock is contended by a blocking lifecycle op
	// (which would re-serialize the whole map). The heavy IO (proc read, file
	// stat) runs afterwards under no lock at all.
	type instPair struct {
		id   vmID
		inst *vmInstance
	}
	s.mu.Lock()
	pairs := make([]instPair, 0, len(s.instances))
	for id, inst := range s.instances {
		pairs = append(pairs, instPair{id: id, inst: inst})
	}
	s.mu.Unlock()

	var inputs []meterInput
	for _, p := range pairs {
		p.inst.mu.Lock()
		if p.inst.state != StateActive {
			p.inst.mu.Unlock()
			continue
		}
		pid := 0
		if p.inst.vm != nil {
			pid = p.inst.vm.PID()
		}
		in := meterInput{
			id:    s.deriveVMConfig(p.id).ID,
			pid:   pid,
			seed:  s.rootfsTemplatePath,
			clone: p.inst.rootfsClonePath,
			tap:   p.inst.activeTap,
		}
		p.inst.mu.Unlock()
		inputs = append(inputs, in)
	}

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
	// Snapshot the instance pointers under s.mu, read each vm handle under its OWN
	// lock, then ping OUTSIDE every lock, so a slow ping never blocks s.mu or a
	// sibling's lifecycle.
	s.mu.Lock()
	insts := make([]*vmInstance, 0, len(s.instances))
	for _, inst := range s.instances {
		insts = append(insts, inst)
	}
	s.mu.Unlock()

	vms := make([]vmm, 0, len(insts))
	for _, inst := range insts {
		inst.mu.Lock()
		vm := inst.vm
		inst.mu.Unlock()
		if vm != nil {
			vms = append(vms, vm)
		}
	}

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
