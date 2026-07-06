package husk

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/metering"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/vsock"
	"mitos.run/mitos/internal/workspace"
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
// Increment 2 scope: the map-based state-machine multiplexing proven with the
// mock VMM: two distinct vmIDs each Prepare -> Dormant -> Activate -> Active
// independently, Close one without disturbing the other, Metering reports every
// active VM. This increment proved the state machine, not a second real
// Firecracker process.
//
// Increment L1.4 scope (this file): activateInstance now programs each vmID's
// OWN in-pod egress filter on a DISTINCT tap + guest IP + gateway + MAC derived
// deterministically from the vmID (deriveVMNetwork / netconf.DeriveInPodSecondaryLink),
// mirroring the single-VM Activate's networking but keyed per instance, so two
// real Firecracker VMs sharing one pod netns never collide on a tap or IP and
// their egress cannot cross. Combined with deriveVMConfig's per-VM socket/workdir
// this lets a REAL second Firecracker come up in the pod. The single-VM path in
// stub.go is byte-for-byte unchanged, and no production caller sets MultiVM (the
// controller is not wired), so nothing shipped changes.
//
// DEFERRED to L1.4b (kept out of this reviewable PR): the per-VM DNS-proxy
// fan-out (several VMs cannot share one ResolverIP:53 listener, so multi-VM
// egress here is IP-allowlist only and name-based egress is off), the fork ops
// (ForkSnapshot / workspace dehydrate-hydrate) keyed by vmID, and a real
// two-tap-in-one-netns nftables egress-isolation integration test on KVM.

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
	if create && s.closing {
		// Refuse EVERY create once teardown has begun, whether or not a map entry
		// already exists. Close resets an instance to StateNew rather than deleting
		// it, so an id can still be present after a prior prepare/close cycle;
		// re-preparing it during teardown would (re)start a VM that outlives Close.
		// A nil return from a create call therefore means "the stub is closing"
		// (prepareInstance maps it to an error).
		return nil
	}
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

// deriveVMNetwork returns the per-VM network identity activateInstance programs
// for one vmID in a multi-VM pod. The default (primary) VM keeps the pod's base
// guest IP, gateway, and MAC, so a multi-VM pod's primary VM programs the same
// link the single-VM path would. Every OTHER vmID is placed on its OWN /30
// point-to-point link within the pod netns, derived deterministically from the
// vmID (netconf.DeriveInPodSecondaryLink), so two VMs in one pod never share a
// guest IP, gateway, MAC, or (since the tap derives from the guest IP) tap, and
// their egress cannot cross. A nil base (the caller programs no networking, e.g.
// the unit path) is returned unchanged.
//
// The returned value is always a COPY; the caller's req.Network is never mutated.
// ResolverIP is cleared for EVERY VM because the per-VM DNS-proxy fan-out is
// deferred to L1.4b: several VMs cannot share one ResolverIP:53 listener, so
// multi-VM egress is IP-allowlist only and no VM is pointed at a resolver that
// has no proxy behind it (fail-closed for name-based egress).
func deriveVMNetwork(id vmID, base *vsock.NotifyForkedNetwork) *vsock.NotifyForkedNetwork {
	if base == nil {
		return nil
	}
	n := *base
	n.ResolverIP = ""
	if id == defaultVMID {
		return &n
	}
	guest, gateway, mac := netconf.DeriveInPodSecondaryLink(string(id))
	n.GuestIP = guest.String()
	n.GatewayIP = gateway.String()
	n.GuestMAC = mac
	// A secondary in-pod link is a /30 point-to-point block.
	n.PrefixLen = 30
	return &n
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
	if inst == nil {
		// instanceFor refused the create because teardown has begun; do not start
		// a VM that would outlive Close.
		return fmt.Errorf("husk: cannot prepare vm %q: stub is closing", id)
	}
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
// The per-VM in-pod egress filter runs here (L1.4): each vmID's VM comes up on
// its OWN tap + guest IP + gateway derived from the vmID (deriveVMNetwork), and
// the snapshot's baked NIC is remapped to that per-VM tap, mirroring the
// single-VM Activate but keyed per instance so two VMs in one pod netns never
// collide. The per-VM DNS-proxy fan-out is DEFERRED to L1.4b: several VMs cannot
// share one ResolverIP:53 listener, so multi-VM egress is IP-allowlist only.
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

	// Per-VM in-pod egress filter. Derive this vmID's OWN /30 (deriveVMNetwork:
	// the primary VM keeps the base link, every secondary gets a distinct guest
	// IP + gateway + MAC + tap), then create the tap and install its default-deny
	// egress chain BEFORE the snapshot load, exactly as the single-VM Activate
	// does: Firecracker requires the host tap to exist at restore time and the
	// baked NIC is remapped to it. Binding the baked NIC to THIS VM's tap keeps
	// the filter and the NIC remap in agreement without a shared allocator. FAIL
	// CLOSED: a filter error means the VM would have unfiltered egress (or a NIC
	// with no backing tap), so it is never loaded. The guest IP and tap carry no
	// secrets. DNS-proxy fan-out is deferred (L1.4b), so no ResolverIP is bound.
	perNet := deriveVMNetwork(id, req.Network)
	overrides := req.NetworkOverrides
	if s.netRunner != nil && perNet != nil {
		tap := netconf.DeriveTapName(perNet.GuestIP)
		// Multi-VM leaves ResolverIP unset (the per-VM DNS proxy fan-out is a later
		// increment); the shared policy fields come from netfilterPolicyConfig.
		cfg := netfilterPolicyConfig(req)
		cfg.Tap = tap
		cfg.GuestIP = net.ParseIP(perNet.GuestIP)
		cfg.HostIP = net.ParseIP(perNet.GatewayIP)
		if err := applyEgressFilter(ctx, s.netRunner, s.enableForwarding, cfg); err != nil {
			werr := fmt.Errorf("husk: apply in-pod egress filter for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
		// Pin the baked NIC to THIS VM's tap so the restored VM's NIC is backed by
		// the tap the filter just created and governed by its egress chain.
		overrides = []firecracker.NetworkOverride{{
			IfaceID:     firecracker.NetIfaceID,
			HostDevName: tap,
		}}
		inst.activeTap = tap
	}

	// Load PAUSED so the rootfs drive can be rebound before the guest runs, then
	// resume explicitly, exactly as the single-VM path does.
	if err := inst.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, overrides); err != nil {
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
	// Deliver THIS VM's derived network (its own guest IP + gateway + MAC) to the
	// guest so it re-addresses eth0 onto the /30 the host-side filter programs,
	// keeping the guest address and the tap/masquerade in agreement per instance.
	notifyReq := req
	notifyReq.Network = perNet
	if err := s.notify(vsockPath, inst.generation, entropy, notifyReq); err != nil {
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

// SpawnVM brings up an ADDITIONAL same-tenant VM (a new vmID) in a running husk
// pod: it prepares a dormant per-VM Firecracker for req.VMID then activates it
// from req.Activate, returning the activated guest's vsock path. It is the server
// side of the spawn-vm control op (netcontrol.go OpSpawnVM).
//
// It FAILS CLOSED on the flag: a stub NOT started with --multi-vm refuses to spawn
// a VM. The single-VM path owns exactly ONE VM and must never be driven to host a
// second, so a spawn-vm misdirected at a single-VM pod is rejected here rather
// than silently starting a second Firecracker. The vmID is validated with
// checkVMID before any path is derived from it (an unsafe or empty id is refused
// up front, before prepareInstance touches the filesystem). Any prepare or
// activate failure returns OK=false with actionable text and leaves no
// half-spawned VM active; prepareInstance and activateInstance are each
// fail-closed per instance and never disturb a sibling.
//
// req.Activate carries the same activation inputs a plain Activate takes,
// including SECRETS (Secrets, Token); those ride the mTLS control channel and are
// NEVER logged here.
func (s *Stub) SpawnVM(ctx context.Context, req SpawnVMRequest) SpawnVMResult {
	if !s.multiVM {
		return SpawnVMResult{
			OK:    false,
			VMID:  req.VMID,
			Error: "husk: spawn-vm refused: this pod is not running in multi-VM mode",
		}
	}
	id := vmID(req.VMID)
	if err := checkVMID(id); err != nil {
		return SpawnVMResult{OK: false, VMID: req.VMID, Error: err.Error()}
	}
	if err := s.prepareInstance(ctx, id); err != nil {
		return SpawnVMResult{
			OK:    false,
			VMID:  req.VMID,
			Error: fmt.Errorf("husk: spawn-vm prepare vm %q: %w", req.VMID, err).Error(),
		}
	}
	res, _ := s.activateInstance(ctx, id, req.Activate)
	return SpawnVMResult{
		OK:            res.OK,
		VMID:          req.VMID,
		VsockPath:     res.VsockPath,
		LatencyMs:     res.LatencyMs,
		Error:         res.Error,
		AlreadyActive: res.AlreadyActive,
	}
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
	// Mark closing BEFORE snapshotting so any concurrent prepareInstance create is
	// refused (instanceFor returns nil) and cannot add a VM after this snapshot that
	// would outlive Close.
	s.closing = true
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

// resumeInstanceAfterFork resumes ONE per-VM instance's source VM after a fork
// snapshot with the SAME bounded retry the single-VM resumeSourceAfterFork uses,
// but scoped to inst.vm: a TRANSIENT resume error must not leave a tenant's live
// source frozen (the v1.24.1 stuck-paused incident). It retries resumeMaxAttempts
// times, resumeRetryBackoff apart, returns nil as soon as one resume succeeds, and
// on total failure emits a distinct error-level log and fires the onSourceLeftPaused
// marker so a source left paused is observable. The caller holds inst.mu.
func (s *Stub) resumeInstanceAfterFork(id vmID, inst *vmInstance) error {
	var err error
	for attempt := 0; attempt < resumeMaxAttempts; attempt++ {
		if attempt > 0 {
			s.backoffSleep(resumeRetryBackoff)
		}
		if err = inst.vm.Resume(); err == nil {
			return nil
		}
	}
	fmt.Fprintf(os.Stderr, "husk: source vm %q left paused after fork snapshot: resume failed after %d attempts: %v\n", id, resumeMaxAttempts, err)
	if s.onSourceLeftPaused != nil {
		s.onSourceLeftPaused()
	}
	return err
}

// forkSnapshotInstance snapshots ONE per-VM instance's running VM, scoped to one
// entry in the instances map. It is the per-VM analog of the single-VM
// ForkSnapshot and mirrors it exactly (pause -> create snapshot -> freeze rootfs
// inside the paused window -> ALWAYS resume the source), but gates on inst.state
// and drives inst.vm.
//
// This is the fix for the L1.8 prod canary: under --multi-vm the CLAIM path
// advanced the DEFAULT instance's state to Active (activateInstance), NOT the
// single-VM s.state, which stays StateNew. The single-VM ForkSnapshot's
// `s.state != StateActive` gate therefore saw StateNew and refused EVERY fork of a
// multi-vm source with "fork-snapshot in state new: must be active", timing out
// the hosted fork loop. Routing through the default instance reads the state
// Activate set, exactly as Metering/Close/pingVMM already multiplex.
//
// FAIL CLOSED: it requires inst StateActive; a pause, snapshot-create, rootfs-freeze,
// or resume failure returns OK=false plus an error, and on a snapshot or freeze
// failure it still attempts to resume the source so a transient error never leaves a
// live sandbox frozen. The fork id and snapshot paths carry no secrets.
func (s *Stub) forkSnapshotInstance(ctx context.Context, id vmID, req ForkSnapshotRequest) (ForkSnapshotResult, error) {
	if err := checkVMID(id); err != nil {
		return ForkSnapshotResult{OK: false, Error: err.Error()}, err
	}
	// Fork does NOT create an instance: a missing entry reads as StateNew and is
	// refused, mirroring activateInstance.
	inst := s.instanceFor(id, false)
	if inst == nil {
		werr := fmt.Errorf("husk: fork-snapshot vm %q in state %s: must be active", id, StateNew)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.state != StateActive {
		return ForkSnapshotResult{OK: false, Error: fmt.Sprintf("fork-snapshot vm %q in state %s: must be active", id, inst.state)},
			fmt.Errorf("husk: fork-snapshot vm %q in state %s: must be active", id, inst.state)
	}
	if err := ctx.Err(); err != nil {
		return ForkSnapshotResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ForkSnapshotResult{OK: false, Error: "fork-snapshot: empty snapshot dir"},
			fmt.Errorf("husk: fork-snapshot vm %q: empty snapshot dir", id)
	}
	if err := s.confineToForksDir(req.SnapshotDir); err != nil {
		return ForkSnapshotResult{OK: false, Error: err.Error()}, err
	}

	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")
	if err := os.MkdirAll(req.SnapshotDir, 0o755); err != nil {
		werr := fmt.Errorf("husk: create fork snapshot dir %s: %w", req.SnapshotDir, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	start := time.Now()

	if err := inst.vm.Pause(); err != nil {
		werr := fmt.Errorf("husk: pause source vm %q for fork snapshot: %w", id, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	if err := inst.vm.CreateSnapshot(memFile, vmStateFile); err != nil {
		// Best effort: resume the source so a transient snapshot error does not leave
		// a tenant's live sandbox frozen. The snapshot already failed, so the resume
		// error is not surfaced, but the bounded retry + stuck-paused marker still
		// apply here (a transient resume blip on this branch must not silently leave
		// the source paused, the v1.24.1 incident class).
		_ = s.resumeInstanceAfterFork(id, inst)
		werr := fmt.Errorf("husk: create fork snapshot in %s for vm %q: %w", req.SnapshotDir, id, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	// Freeze THIS instance's source rootfs INSIDE the paused window, as a
	// point-in-time pair with the mem+vmstate checkpoint, exactly as the single-VM
	// ForkSnapshot does. Skipped when this instance has no per-activation clone (the
	// mock/CI paths). On failure the source is resumed (never leave a tenant's live
	// sandbox frozen) before failing closed. The path carries no secret.
	if inst.rootfsClonePath != "" {
		frozenRootfs := filepath.Join(req.SnapshotDir, "rootfs.ext4")
		if err := s.reflink(inst.rootfsClonePath, frozenRootfs); err != nil {
			_ = s.resumeInstanceAfterFork(id, inst)
			werr := fmt.Errorf("husk: freeze source rootfs for fork snapshot vm %q: %w", id, err)
			return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// ALWAYS resume the source after the checkpoint: the pause is only the brief
	// quiescence CreateSnapshot requires, and the memory + frozen rootfs are a
	// consistent point-in-time pair, so the source is safe to run again. Leaving it
	// paused was the v1.24.1 production bug. The resume is retried a few times so a
	// transient blip does not recreate that stuck-paused incident.
	if err := s.resumeInstanceAfterFork(id, inst); err != nil {
		werr := fmt.Errorf("husk: resume source vm %q after fork snapshot: %w", id, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	latency := time.Since(start)
	return ForkSnapshotResult{
		OK:          true,
		SnapshotDir: req.SnapshotDir,
		LatencyMs:   float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// dialWorkspaceAgentVM resolves the guest-agent bulk-tar transport for a SPECIFIC
// per-VM handle (an instance's inst.vm under --multi-vm) and returns it plus a
// close hook. It is the per-VM analog of the single-VM dialWorkspaceAgent, which
// reads s.vm; under --multi-vm s.vm is nil and the VM lives on the instance, so the
// workspace ops resolve the transport from inst.vm here instead. The caller holds
// the instance lock.
func (s *Stub) dialWorkspaceAgentVM(vm vmm) (workspace.VsockTransport, func(), error) {
	vsockPath := vm.VsockHostPath(s.vsockRelPath)
	agent, err := s.wsTransport(vsockPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("husk: connect guest agent for workspace transfer: %w", err)
	}
	closeHook := func() {}
	if c, ok := agent.(io.Closer); ok {
		closeHook = func() { _ = c.Close() }
	}
	return agent, closeHook, nil
}

// dehydrateWorkspaceInstance captures ONE per-VM instance's guest /workspace into
// the node CAS, scoped to one entry in the instances map. It is the per-VM analog
// of the single-VM DehydrateWorkspace and mirrors it exactly, but gates on
// inst.state and dials inst.vm. Same L1.8 fix class as forkSnapshotInstance: the
// single-VM DehydrateWorkspace gates on s.state (StateNew under --multi-vm) and
// dials s.vm (nil), so on a multi-vm pod it refused with "dehydrate-workspace in
// state new: must be active"; routing through the default instance uses the state
// and VM Activate set.
//
// FAIL CLOSED: it requires inst StateActive and a configured node CAS. The manifest
// digest is a content address, NOT a secret; workspace CONTENT bytes are never
// logged. The instance stays StateActive throughout.
func (s *Stub) dehydrateWorkspaceInstance(ctx context.Context, id vmID, req DehydrateWorkspaceRequest) (DehydrateWorkspaceResult, error) {
	if err := checkVMID(id); err != nil {
		return DehydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	inst := s.instanceFor(id, false)
	if inst == nil {
		werr := fmt.Errorf("husk: dehydrate-workspace vm %q in state %s: must be active", id, StateNew)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.state != StateActive {
		werr := fmt.Errorf("husk: dehydrate-workspace vm %q in state %s: must be active", id, inst.state)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := ctx.Err(); err != nil {
		return DehydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	if s.casStore == nil {
		werr := fmt.Errorf("husk: dehydrate-workspace: no node CAS configured; set --cas-dir so the stub can persist a workspace revision")
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	agent, closeAgent, err := s.dialWorkspaceAgentVM(inst.vm)
	if err != nil {
		return DehydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	defer closeAgent()

	start := time.Now()
	digest, err := workspace.Dehydrate(ctx, agent, s.casStore, req.ExcludePaths, req.CapturePaths)
	if err != nil {
		werr := fmt.Errorf("husk: dehydrate workspace: %w", err)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := digest.Validate(); err != nil {
		werr := fmt.Errorf("husk: dehydrate workspace produced an invalid content digest: %w", err)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	// Optional content-hash diff against the parent head, computed from the two
	// manifests in the node CAS via the SAME stub-level helper the single-VM path
	// uses; it never materializes chunk bytes. An error names manifests/digests
	// (content addresses), never content.
	var diff *workspace.Diff
	if req.ParentManifestDigest != "" {
		parent := cas.Digest(req.ParentManifestDigest)
		if err := parent.Validate(); err != nil {
			werr := fmt.Errorf("husk: dehydrate workspace: invalid parent manifest digest: %w", err)
			return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
		}
		d, derr := s.diffManifests(parent, digest)
		if derr != nil {
			werr := fmt.Errorf("husk: dehydrate workspace: compute diff against parent: %w", derr)
			return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
		}
		diff = &d
	}

	latency := time.Since(start)
	return DehydrateWorkspaceResult{
		OK:             true,
		ManifestDigest: string(digest),
		Diff:           diff,
		LatencyMs:      float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// hydrateWorkspaceInstance restores a node-CAS manifest into ONE per-VM instance's
// guest /workspace, the inverse of dehydrateWorkspaceInstance and the per-VM analog
// of the single-VM HydrateWorkspace. Same L1.8 fix class: it gates on inst.state
// and dials inst.vm instead of s.state / s.vm.
//
// FAIL CLOSED: it requires inst StateActive, a configured node CAS, and a valid
// content-address manifest digest. Workspace CONTENT bytes are never logged. The
// instance stays StateActive throughout.
func (s *Stub) hydrateWorkspaceInstance(ctx context.Context, id vmID, req HydrateWorkspaceRequest) (HydrateWorkspaceResult, error) {
	if err := checkVMID(id); err != nil {
		return HydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	inst := s.instanceFor(id, false)
	if inst == nil {
		werr := fmt.Errorf("husk: hydrate-workspace vm %q in state %s: must be active", id, StateNew)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.state != StateActive {
		werr := fmt.Errorf("husk: hydrate-workspace vm %q in state %s: must be active", id, inst.state)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := ctx.Err(); err != nil {
		return HydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	if s.casStore == nil {
		werr := fmt.Errorf("husk: hydrate-workspace: no node CAS configured; set --cas-dir so the stub can restore a workspace revision")
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	digest := cas.Digest(req.ManifestDigest)
	if err := digest.Validate(); err != nil {
		werr := fmt.Errorf("husk: hydrate-workspace: %w", err)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	agent, closeAgent, err := s.dialWorkspaceAgentVM(inst.vm)
	if err != nil {
		return HydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	defer closeAgent()

	start := time.Now()
	if err := workspace.Hydrate(ctx, agent, s.casStore, digest); err != nil {
		werr := fmt.Errorf("husk: hydrate workspace: %w", err)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	latency := time.Since(start)
	return HydrateWorkspaceResult{
		OK:        true,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}
