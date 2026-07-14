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
	"sort"
	"strings"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/guestgrpc"
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

// stageMs returns the milliseconds elapsed since t, the unit the fork stage
// breakdown records (matching LatencyMs). It is a pure timing helper; it records
// no secret.
func stageMs(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

// formatStages renders a stage map as a deterministic "name=1.23 name=4.56"
// string (keys sorted) for a single-line log. It carries only stage names and
// millisecond durations, never a secret.
func formatStages(stages map[string]float64) string {
	if len(stages) == 0 {
		return ""
	}
	names := make([]string, 0, len(stages))
	for name := range stages {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%.2f", name, stages[name])
	}
	return b.String()
}

// recordStage stores dur under name in stages when stages is non-nil. The stage
// maps are the per-stage fork breakdown the controller logs and observes; a nil
// map (a caller that did not ask for timing) is a no-op so the fast path stays
// allocation-free. name is a fixed vocabulary value, never a secret or an id.
func recordStage(stages map[string]float64, name string, since time.Time) {
	if stages != nil {
		stages[name] = stageMs(since)
	}
}

// prepareInstance brings up a DORMANT Firecracker VMM for one vmID and records it
// on the per-VM instance, allocating the instance on first use. It mirrors the
// single-VM Prepare (state gate, snapshot verify at prepare, per-activation rootfs
// clone) but scoped to ONE entry in the instances map, so preparing a second VM
// never touches the first. Fail closed: any error tears the dormant VMM down and
// leaves the instance out of StateDormant.
//
// rootfsSrcOverride selects the file the per-activation rootfs CoW clone is made
// FROM. Empty (every template activate) clones from the pool template rootfs
// (s.rootfsTemplatePath), the prior behavior byte-for-byte. A CO-LOCATED fork child
// passes the FROZEN source rootfs the fork snapshot carries (SnapshotDir/rootfs.ext4)
// so the child inherits the parent's DISK instead of the pristine template, the
// co-located analog of the new-pod fork child's --rootfs pointing at the frozen
// source rootfs.
// extraEnv (variadic, milestone m5) is appended to the child Firecracker process
// environment at launch. SpawnVM passes the FIRECRACKER_MITOS_CHILD_MEMFD entry
// here so a co-located live-cow child boots its guest RAM from the parent's shared
// memfd; every other caller passes none, preserving the prior behavior.
// stages, when non-nil, is filled with this prepare's per-stage timing (fc_boot,
// verify_prepare, rootfs_clone) in milliseconds; a nil map disables timing with
// no allocation. It is written under the instance lock, so a caller reads it only
// after prepareInstance returns. Timing/observability only.
func (s *Stub) prepareInstance(ctx context.Context, id vmID, rootfsSrcOverride string, stages map[string]float64, extraEnv ...string) error {
	return s.prepareInstanceOpt(ctx, id, prepareOpts{
		rootfsSrcOverride: rootfsSrcOverride,
		stages:            stages,
		extraEnv:          extraEnv,
	})
}

// prepareOpts carries the optional inputs prepareInstanceOpt varies over. The
// zero value (what prepareInstance forwards) is a plain fresh prepare: start a
// dormant VMM, verify the template snapshot when configured, clone the
// per-activation rootfs.
type prepareOpts struct {
	// rootfsSrcOverride selects the file the per-activation rootfs CoW clone is
	// made FROM. Empty clones from the pool template rootfs (s.rootfsTemplatePath);
	// a co-located fork child passes the frozen source rootfs the fork snapshot
	// carries (SnapshotDir/rootfs.ext4).
	rootfsSrcOverride string
	// skipRootfsClone leaves the instance WITHOUT a per-activation rootfs clone,
	// used by the pre-warm boot to keep a GENERIC dormant child. The child's
	// fork-time rootfs is cloned later, at consume, from the fork snapshot it
	// actually carries, so the reflink stays at fork time (never on the pre-warm).
	skipRootfsClone bool
	// reuseVM, when non-nil, ADOPTS an already-booted dormant VMM (the pre-warmed
	// child) into this instance instead of starting a fresh Firecracker. The
	// process boot (fc_boot) and the template snapshot verify were paid during the
	// pre-warm, off the hot path, so both are skipped here and fc_boot is recorded
	// as 0. extraEnv is ignored (the VMM is already launched).
	reuseVM vmm
	// stages, when non-nil, is filled with this prepare's per-stage timing.
	stages map[string]float64
	// extraEnv is appended to the child Firecracker process environment at launch
	// (the m5 child memfd-import env). It never applies to an adopted VMM.
	extraEnv []string
}

// prepareInstanceOpt is prepareInstance's core, parameterized by prepareOpts so
// ONE fail-closed, per-instance-locked state machine backs three callers: a plain
// fresh prepare (SpawnVM on-demand, the single-VM Prepare), the pre-warm BOOT
// (skipRootfsClone: a generic dormant child, no rootfs clone), and the pre-warm
// CONSUME (reuseVM: adopt the pre-warmed VMM, skipping fc_boot + the template
// verify, still cloning THIS fork's rootfs). Fail closed: any error tears the
// (started or adopted) VMM down and leaves the instance out of StateDormant. It
// never reads or mutates a sibling instance.
func (s *Stub) prepareInstanceOpt(ctx context.Context, id vmID, opts prepareOpts) error {
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
	// Append any per-spawn environment (m5: the child memfd-import env) onto a COPY
	// of the base env so one spawn's env never mutates the shared s.cfg slice.
	if len(opts.extraEnv) > 0 {
		cfg.Env = append(append([]string(nil), cfg.Env...), opts.extraEnv...)
	}
	// Arm the PARENT side of the live-cow fork for the SOURCE (default) VM (milestone
	// m6b): bind the write-protect handshake socket and launch THIS Firecracker with
	// the FIRECRACKER_MITOS_* env so it exports its guest memfd (m1) and offers the
	// write-protect uffd (m2) to the handler. Once the handshake completes the freezer
	// is armed (SetLiveCowParent) and forkSnapshotInstance takes the vmstate-only
	// snapshot path (skipping the ~364ms mem write) instead of the Full fallback. Gated
	// inside armLiveCowSource on --live-cow-fork AND a real workdir, so every child VM,
	// the disk path, and the mock/unit path launch stock (a fork never breaks).
	if id == defaultVMID {
		if parentEnv := s.armLiveCowSource(cfg.WorkDir); len(parentEnv) > 0 {
			cfg.Env = append(append([]string(nil), cfg.Env...), parentEnv...)
		}
	}
	// Create the per-VM workdir before launching so the Firecracker API socket and
	// vsock UDS have a home. The default VM reuses the pod workdir (already created
	// by cmd/husk-stub), and the unit path has an empty workdir, so only a nested
	// non-default VM needs the mkdir here.
	if cfg.WorkDir != "" && id != defaultVMID {
		if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
			return fmt.Errorf("husk: create per-VM workdir for vm %q: %w", id, err)
		}
	}
	if opts.reuseVM != nil {
		// ADOPT the pre-warmed dormant VMM. Its process boot AND the template
		// snapshot verify were paid during the pre-warm, off the hot path, so the
		// fork path skips both here. fc_boot is recorded as 0 to make the "boot was
		// pre-paid" fact legible in the merged stage breakdown. The adopted VMM was
		// booted with a GENERIC config (no fork-specific env), so it only serves a
		// fork that needs no launch env (the live-cow child-import fork, which needs
		// FIRECRACKER_MITOS_CHILD_MEMFD at exec, is never routed here).
		inst.vm = opts.reuseVM
		if opts.stages != nil {
			opts.stages["fc_boot"] = 0
		}
	} else {
		fcBootStart := time.Now()
		vm, err := s.start(cfg)
		if err != nil {
			return fmt.Errorf("husk: prepare dormant VMM for vm %q: %w", id, err)
		}
		recordStage(opts.stages, "fc_boot", fcBootStart)
		inst.vm = vm

		// Snapshot verify at prepare time (pre-paid, dormant), the same fail-closed
		// gate the single-VM Prepare runs, when the pod was started with the snapshot
		// dir + expected digest. The snapshot is read-only and content-addressed, so
		// verifying once here is equivalent to verifying at activate.
		if s.prepareSnapshotDir != "" && s.prepareExpectedDigest != "" {
			verifyStart := time.Now()
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
			recordStage(opts.stages, "verify_prepare", verifyStart)
			inst.prepareVerified = true
		}
	}

	// Per-activation rootfs CoW clone, scoped to this VM's derived id so two VMs in
	// the pod never share or overwrite a clone. Skipped for the pre-warm boot: a
	// generic dormant child clones its fork-time rootfs at consume, not here.
	if !opts.skipRootfsClone {
		if err := s.cloneRootfsForInstance(ctx, id, cfg, inst, opts.rootfsSrcOverride, opts.stages); err != nil {
			_ = inst.vm.Close()
			inst.vm = nil
			return err
		}
	}

	// Bring THIS pod's default-VM tap up while it is dormant, under a DEFAULT-DENY
	// policy, so the claim pays only the atomic nft transaction that installs the
	// tenant's policy. Opt-in (Options.PrepareEgressLink), default VM only, and only
	// when the in-pod link is configured; otherwise activate does the whole thing as
	// before. FAIL CLOSED: ensureEgressLink tears the tap down on any error and we
	// refuse to go dormant, so the pool never offers a pod whose datapath is
	// half-built. A dormant tap is never a hazard: nothing is loaded behind it, and
	// the policy on it denies everything.
	if err := s.prepareEgressLinkFor(ctx, id, inst); err != nil {
		if inst.vm != nil {
			_ = inst.vm.Close()
			inst.vm = nil
		}
		return err
	}

	// Load the snapshot and RESUME the guest while dormant, so the claim pays only the
	// fork-correctness handshake. Requires the tap prepared just above (Firecracker
	// binds the baked NIC to it at restore). FAIL CLOSED: on any error we refuse to go
	// dormant, so the pool never offers a half-restored pod.
	if err := s.prepareRestoreDefaultVM(ctx, id, inst, opts); err != nil {
		if inst.vm != nil {
			_ = inst.vm.Close()
			inst.vm = nil
		}
		return err
	}

	inst.state = StateDormant
	return nil
}

// prepareRestoreDefaultVM loads the pod's default-VM snapshot and resumes its guest
// while the pod is DORMANT, when the stub opted in (Options.PrepareRestore). It is the
// same load/rootfs-rebind/resume/guest-ready the warm-claim activate does, moved off the
// hot path; activate then skips it and pays only the handshake.
//
// A NO-OP unless every precondition holds: PrepareRestore on, the tap was prepared just
// above (preparedLinkTap set), the DEFAULT vm only, a real snapshot dir configured, and
// this is not the pre-warm generic-child boot (opts.skipRootfsClone) or an adopted VMM
// (opts.reuseVM). A co-located fork
// child (its own snapshot, childUFFD backend, per-fork tap) is never pre-restored here;
// it restores at spawn/activate exactly as before.
//
// The guest agent conn proven by the readiness wait is CLOSED, not held across dormancy:
// a long-idle vsock conn can go stale, and re-dialing an already-running guest at the
// claim is a sub-millisecond round trip.
func (s *Stub) prepareRestoreDefaultVM(ctx context.Context, id vmID, inst *vmInstance, opts prepareOpts) error {
	// id != defaultVMID already excludes the pre-warm generic child (it is always a
	// secondary VM); skipRootfsClone and reuseVM are belt-and-braces so a future
	// pre-warm of the default slot never double-resumes an adopted VMM.
	if !s.prepareRestore || id != defaultVMID || inst.preparedLinkTap == "" ||
		s.prepareSnapshotDir == "" || opts.skipRootfsClone || opts.reuseVM != nil {
		return nil
	}
	// PrepareRestore requires PrepareEgressLink; the stub, pod builder, and controller
	// all enforce that, and preparedLinkTap being set is the runtime proof the tap is up.
	start := time.Now()
	memFile := filepath.Join(s.prepareSnapshotDir, "mem")
	vmStateFile := filepath.Join(s.prepareSnapshotDir, "vmstate")
	// The baked NIC is remapped onto the prepared tap, the same override activate builds
	// for the default VM (whose guest IP is the fixed in-pod constant).
	overrides := []firecracker.NetworkOverride{{
		IfaceID:     firecracker.NetIfaceID,
		HostDevName: inst.preparedLinkTap,
	}}
	if err := s.setLiveCowMemSource(memFile); err != nil {
		return fmt.Errorf("husk: prepare-restore arm lazy live-cow mem source for vm %q: %w", id, err)
	}
	if err := inst.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, overrides); err != nil {
		return fmt.Errorf("husk: prepare-restore load snapshot for vm %q: %w", id, err)
	}
	if inst.rootfsClonePath != "" {
		if err := inst.vm.PatchDrive("rootfs", inst.rootfsClonePath); err != nil {
			return fmt.Errorf("husk: prepare-restore rebind rootfs drive for vm %q: %w", id, err)
		}
	}
	if err := inst.vm.Resume(); err != nil {
		return fmt.Errorf("husk: prepare-restore resume vm %q: %w", id, err)
	}
	conn, err := s.ready(ctx, inst.vm.VsockHostPath(firecracker.VsockRelPath), s.readyTimeout)
	if err != nil {
		return fmt.Errorf("husk: prepare-restore guest not ready for vm %q: %w", id, err)
	}
	if conn != nil {
		_ = conn.Close() //nolint:errcheck // do not hold a vsock conn across dormancy
	}
	inst.preRestored = true
	inst.preRestoredSnapshotDir = s.prepareSnapshotDir
	// The one observable marker that the dormant restore happened and what it cost;
	// the KVM CI gate and the prod canary both key on this line. Paths and durations
	// only, never secrets.
	fmt.Fprintf(os.Stderr, "husk: prepare-restore vm %q resumed dormant guest from %s in %.2fms\n",
		id, s.prepareSnapshotDir, float64(time.Since(start).Microseconds())/1000.0)
	return nil
}

// prepareEgressLinkFor brings up the default VM's tap with a default-deny policy while
// the pod is dormant, when the stub opted in. A no-op otherwise, and never for a
// secondary (co-located fork child) VM, whose tap is derived per fork at activate.
func (s *Stub) prepareEgressLinkFor(ctx context.Context, id vmID, inst *vmInstance) error {
	// !s.multiVM is implied today (only the multi-VM Prepare reaches prepareInstance),
	// but it is stated rather than relied upon: the single-VM Activate path does not
	// consult preparedLinkTap, so a dormant tap it never reuses would be dead weight.
	if !s.prepareEgressLink || !s.multiVM || id != defaultVMID || s.netRunner == nil || s.inPodGuestIP == "" {
		return nil
	}
	tap := netconf.DeriveTapName(s.inPodGuestIP)
	cfg := NetfilterConfig{
		Tap:     tap,
		GuestIP: net.ParseIP(s.inPodGuestIP),
		HostIP:  net.ParseIP(s.inPodGatewayIP),
		Egress:  v1.EgressDeny,
	}
	if err := ensureEgressLink(ctx, s.netRunner, s.enableForwarding, cfg); err != nil {
		return fmt.Errorf("husk: prepare in-pod egress link for vm %q: %w", id, err)
	}
	if err := applyEgressPolicy(ctx, s.netRunner, cfg); err != nil {
		return fmt.Errorf("husk: prepare default-deny egress policy for vm %q: %w", id, err)
	}
	// Record it on BOTH fields: activeTap so Close tears it down even if this pod is
	// never claimed, preparedLinkTap so activate knows it can skip the link setup.
	inst.activeTap = tap
	inst.preparedLinkTap = tap
	return nil
}

// cloneRootfsForInstance makes THIS VM's per-activation rootfs CoW clone and
// records it on the instance so activate rebinds the drive to it and Close removes
// it. The clone SOURCE is the pool template rootfs by default; a co-located fork
// child overrides it with the frozen source rootfs the fork snapshot carries so
// the child inherits the parent's DISK. A no-op (nil) when no template rootfs or
// CoW dir is configured (the mock/unit path). It NEVER tears the VMM down on
// error; the caller owns that so the fail-closed teardown stays in one place.
func (s *Stub) cloneRootfsForInstance(ctx context.Context, id vmID, cfg firecracker.VMConfig, inst *vmInstance, rootfsSrcOverride string, stages map[string]float64) error {
	rootfsSrc := s.rootfsTemplatePath
	if rootfsSrcOverride != "" {
		rootfsSrc = rootfsSrcOverride
	}
	if rootfsSrc == "" || s.rootfsCoWDir == "" {
		return nil
	}
	clonePath := filepath.Join(s.rootfsCoWDir, cfg.ID, "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return fmt.Errorf("husk: create per-activation rootfs dir for vm %q: %w", id, err)
	}
	if err := waitForFile(ctx, rootfsSrc, rootfsTemplateWait); err != nil {
		return fmt.Errorf("husk: per-activation rootfs source %s not ready for vm %q: %w", rootfsSrc, id, err)
	}
	cloneStart := time.Now()
	if err := s.reflink(rootfsSrc, clonePath); err != nil {
		return fmt.Errorf("husk: clone per-activation rootfs for vm %q: %w", id, err)
	}
	recordStage(stages, "rootfs_clone", cloneStart)
	inst.rootfsClonePath = clonePath
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
	// Pre-restored snapshot IDENTITY gate, FIRST, before any side effect. A resumed
	// guest cannot be reloaded, so a claim naming a different snapshot than the one this
	// pod pre-restored must be refused BEFORE the verify-on-activate re-hash runs and
	// BEFORE the tenant egress policy is installed on the tap fronting the (wrong) guest.
	// Hoisting it here (rather than at the pre-restored fast path below) is what makes
	// the refusal genuinely fail-closed: no network mutation, no re-verify, nothing.
	if inst.preRestored && req.SnapshotDir != inst.preRestoredSnapshotDir {
		werr := fmt.Errorf("husk: activate vm %q wants snapshot %q but this pod pre-restored %q; refusing to serve the wrong image", id, req.SnapshotDir, inst.preRestoredSnapshotDir)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")

	start := time.Now()
	// Per-stage timing of the activate, so the controller can attribute WHAT
	// dominates a hosted fork instead of seeing only the total. Each block below
	// records its own elapsed under a fixed stage name; mark is reset after each.
	// Timing/observability only, under the instance lock this method already holds.
	stages := make(map[string]float64, 6)
	mark := time.Now()

	// Verify-on-activate gate, with the same prepare-time fast path the single-VM
	// Activate uses: skip the re-hash only when THIS instance verified this exact
	// snapshot during its dormant period. A CO-LOCATED fork child (req.ForkSnapshot)
	// restores a node-local FORK snapshot the source stub produced in the SAME
	// pod/node trust boundary; it is NOT content-addressed (there is no recorded
	// digest to verify against), so the content-addressed verify is skipped exactly
	// as the new-pod fork child does with --allow-unverified-snapshots. The
	// fork-correctness RNG/clock reseed handshake below still runs, fail-closed.
	switch {
	case req.ForkSnapshot:
		// node-local fork snapshot: skip the content-addressed verify (no digest).
	case !(inst.prepareVerified && req.SnapshotDir == s.prepareSnapshotDir && req.ExpectedDigest == s.prepareExpectedDigest):
		if err := s.verify(req); err != nil {
			werr := fmt.Errorf("husk: snapshot verification failed for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}
	recordStage(stages, "verify", mark)
	mark = time.Now()

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
		// When this instance brought the SAME tap up while dormant, the claim pays only
		// the nft transaction that REPLACES the default-deny policy with the tenant's.
		// nft applies it atomically, so there is no window in which the VM is
		// unfiltered. Any other tap (a fork child's, or a pod that did not opt in) takes
		// the full path. Both halves fail closed by tearing the tap down, so clear the
		// prepared marker first: after a failure the link no longer exists and the next
		// attempt must rebuild it.
		prepared := inst.preparedLinkTap != "" && inst.preparedLinkTap == tap
		inst.preparedLinkTap = ""
		var ferr error
		if prepared {
			ferr = applyEgressPolicy(ctx, s.netRunner, cfg)
		} else {
			ferr = applyEgressFilter(ctx, s.netRunner, s.enableForwarding, cfg)
		}
		if ferr != nil {
			inst.activeTap = ""
			werr := fmt.Errorf("husk: apply in-pod egress filter for vm %q: %w", id, ferr)
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
	recordStage(stages, "egress_filter", mark)
	mark = time.Now()

	// PRE-RESTORED fast path: this instance loaded its snapshot and resumed its guest
	// while dormant (Options.PrepareRestore), so skip the restore, the rootfs rebind,
	// the resume, and the guest-ready wait, and pay only the handshake below. FAIL
	// CLOSED on a snapshot mismatch: a resumed guest cannot be reloaded, so if the claim
	// names a different snapshot than the one restored at prepare, refuse rather than
	// serve the wrong image. Only the default VM is ever pre-restored (see
	// prepareRestoreDefaultVM), and a co-located fork child is never pre-restored.
	if inst.preRestored {
		// The snapshot-identity gate ran fail-closed at the top of activate, before the
		// egress policy, so by here req.SnapshotDir == inst.preRestoredSnapshotDir.
		recordStage(stages, "vmstate_restore", mark)
		recordStage(stages, "resume", mark)
		// Re-dial the already-running guest for the handshake. Sub-millisecond: the
		// guest agent's vsock listener has been up since the prepare-time resume.
		vsockPath := inst.vm.VsockHostPath(firecracker.VsockRelPath)
		guestConn, err := s.ready(ctx, vsockPath, s.readyTimeout)
		if err != nil {
			werr := fmt.Errorf("husk: pre-restored guest not reachable at activate for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
		if guestConn != nil {
			defer guestConn.Close() //nolint:errcheck // best-effort on close
		}
		recordStage(stages, "guest_ready", mark)
		return s.activateHandshakeAndServe(ctx, id, inst, req, perNet, vsockPath, guestConn, stages, start)
	}

	// LAZY live-cow restore (this is what makes vmstate_restore O(1) instead of an
	// eager 512 MiB memfd copy inside PUT /snapshot/load): hand the armed WP handler
	// the mem file BEFORE the load, so it can serve the MISSING faults the patched
	// Firecracker will take for every guest page. Only the SOURCE VM restores from a
	// disk mem file; a co-located fork child imports the parent's memfd and never
	// reaches the lazy branch. FAIL CLOSED: the source was launched with
	// EnvLazyRestore, so a handler that cannot read the mem file means guest RAM would
	// stay zeroed; refuse to load rather than run a VM on empty memory.
	if id == defaultVMID && !req.ForkSnapshot {
		if err := s.setLiveCowMemSource(memFile); err != nil {
			werr := fmt.Errorf("husk: arm lazy live-cow mem source for vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// Load PAUSED so the rootfs drive can be rebound before the guest runs, then
	// resume explicitly, exactly as the single-VM path does. A co-located live-cow
	// fork child armed with a lazy-UFFD import (childUFFDPlan) restores through
	// Firecracker's NATIVE Uffd backend so it faults its guest RAM in ON DEMAND
	// (composed from the source memfd + FROZEN overlay) instead of reading a disk mem
	// file; every other VM loads the disk mem. On the lazy path there is no disk mem
	// (the fork went vmstate-only), so a UFFD load failure is fatal for this spawn
	// (fail-closed, never serve a half-restored guest); the plan is only set when the
	// parent armed, so it is never taken for a plain disk fork.
	if plan := inst.childUFFDPlan; plan != nil {
		inst.childUFFDPlan = nil
		handler, err := s.loadSnapshotChildUFFD(inst.vm, plan, vmStateFile, overrides)
		if err != nil {
			werr := fmt.Errorf("husk: lazy-uffd load snapshot from %s for vm %q: %w", req.SnapshotDir, id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
		inst.childUFFD = handler
	} else if err := inst.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, overrides); err != nil {
		werr := fmt.Errorf("husk: load snapshot from %s for vm %q: %w", req.SnapshotDir, id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	recordStage(stages, "vmstate_restore", mark)
	mark = time.Now()
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
	recordStage(stages, "resume", mark)
	mark = time.Now()

	vsockPath := inst.vm.VsockHostPath(firecracker.VsockRelPath)
	guestConn, err := s.ready(ctx, vsockPath, s.readyTimeout)
	if err != nil {
		werr := fmt.Errorf("husk: guest not ready after activate for vm %q: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	// Reuse the connection the readiness probe proved healthy for the handshake below,
	// instead of dialing the guest over vsock a second time. We own the Close.
	if guestConn != nil {
		defer guestConn.Close() //nolint:errcheck // best-effort on close
	}
	recordStage(stages, "guest_ready", mark)

	return s.activateHandshakeAndServe(ctx, id, inst, req, perNet, vsockPath, guestConn, stages, start)
}

// activateHandshakeAndServe runs the CLAIM-specific tail of an activation: the fork-
// correctness handshake (fresh entropy, clock step, per-VM network re-addressing, and
// the tenant secrets), serving the sandbox API with the claim token, and the state and
// timing bookkeeping. It is shared by the normal restore-at-activate path and the
// pre-restored fast path, which reach it with an already-ready guest. mark is the clock
// for the handshake stage; start is the whole-activate clock for the total.
func (s *Stub) activateHandshakeAndServe(ctx context.Context, id vmID, inst *vmInstance, req ActivateRequest, perNet *vsock.NotifyForkedNetwork, vsockPath string, guestConn *guestgrpc.Client, stages map[string]float64, start time.Time) (ActivateResult, error) {
	_ = ctx
	mark := time.Now()
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
	if err := s.notify(guestConn, vsockPath, inst.generation, entropy, notifyReq); err != nil {
		werr := fmt.Errorf("husk: fork-correctness handshake failed for vm %q: %w", id, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	recordStage(stages, "handshake", mark)
	mark = time.Now()

	if s.onActivated != nil {
		if err := s.onActivated(vsockPath, req.Token); err != nil {
			werr := fmt.Errorf("husk: serve sandbox API for activated vm %q: %w", id, err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}
	recordStage(stages, "serve_api", mark)

	latency := time.Since(start)
	// Structured per-stage timing to the stub log (stderr), so a single activate
	// is legible on the node even without the controller-side breakdown. Stage
	// names and durations only; no secret, token, or entropy value is logged.
	fmt.Fprintf(os.Stderr, "husk: activate vm %q stage timing ms: %s total=%.2f\n", id, formatStages(stages), float64(latency.Microseconds())/1000.0)
	inst.state = StateActive
	// A claimed pod is now a live tenant VM whose next request may be a co-located
	// fork. Eagerly boot the pod's ONE dormant child slot so that FIRST fork adopts
	// it (fc_boot=0) instead of paying the boot on its hot path; without this the
	// slot is created only AFTER a fork, so a fresh pod's first fork always misses.
	// Gated on the SOURCE (default) VM: a fork child's own activation must not warm
	// a slot (SpawnVM's post-fork rewarm already replenishes). Off the hot path (a
	// goroutine) and fail-closed, so it never delays activate and never over-admits
	// the per-VM budget (the adopted slot is the same extra VM an on-demand fork
	// would boot). A no-op when pre-warm is off or the pod is single-VM.
	if id == defaultVMID {
		s.eagerPrewarmChildAsync()
	}
	return ActivateResult{
		OK:        true,
		VsockPath: vsockPath,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
		Stages:    stages,
	}, nil
}

// armInstanceChildUFFD stores a co-located fork child's lazy-UFFD import plan on
// its dormant instance so activateInstance takes the Uffd backend at its load step.
// It takes the instance lock (SpawnVM calls it between prepareInstance and
// activateInstance, both of which take the lock themselves); a missing instance is
// a no-op (activate will then fail closed on the missing dormant VM). It arms ONLY
// while the instance is STILL DORMANT: a concurrent Close between prepareInstance
// and here resets it to StateNew (clearing any plan), so arming unconditionally
// could leave a stale plan that a LATER prepare of the same vmID would wrongly
// consume; a state mismatch skips the arm and the spawn takes the disk path (or
// fails closed on a missing dormant VM at activate).
func (s *Stub) armInstanceChildUFFD(id vmID, plan *lazyChildUFFDPlan) {
	inst := s.instanceFor(id, false)
	if inst == nil {
		return
	}
	inst.mu.Lock()
	if inst.state == StateDormant {
		inst.childUFFDPlan = plan
	}
	inst.mu.Unlock()
}

// loadSnapshotChildUFFD restores a co-located fork child through Firecracker's
// native userfaultfd backend and returns a handler already serving the child's
// guest-memory faults from the composed source (the source memfd + FROZEN
// overlay). It binds the backend socket handler, starts the handshake receiver,
// points /snapshot/load at the socket (paused), waits for the handshake, then
// starts the Serve loop so the load's own device-restore faults are filled. On any
// error it Closes the handler and returns. The caller resumes the VM and retains
// the handler on the instance so teardown Closes it. It mirrors the issue #167
// UFFD restore orchestration (uffd_engine.go), minus the hot-page preload: the
// live-cow child pays only the faults for its working set, on demand.
func (s *Stub) loadSnapshotChildUFFD(vm vmm, plan *lazyChildUFFDPlan, vmStateFile string, overrides []firecracker.NetworkOverride) (fork.ChildUFFDHandle, error) {
	h, err := fork.StartChildUFFDHandler(plan.sockPath, plan.imp)
	if err != nil {
		return nil, fmt.Errorf("start child uffd handler: %w", err)
	}
	// Firecracker connects to the socket during /snapshot/load and FAULTS guest
	// memory DURING the load (device restore dereferences guest RAM), blocking in the
	// kernel until the handler services those faults. So the receiver must be
	// accepting before the load, and Serve must be running before the load can
	// complete: start the receiver + the load PUT concurrently, and once the handshake
	// delivers the uffd, start Serve so the load's faults are handled.
	//
	// FAIL-CLOSED, NO HANG: every wait races the OTHER outcomes so no failure mode
	// blocks forever. If the load fails BEFORE Firecracker connects, Receive's Accept
	// would block indefinitely, so a load result that arrives before the handshake
	// tears the handler down (which unblocks Accept). If Serve exits while the load is
	// still faulting, the load is stuck on a page no one will fill, so a Serve result
	// during the load also fails closed. All channels are buffered so a goroutine never
	// blocks on send after we stop reading.
	recvErr := make(chan error, 1)
	go func() { recvErr <- h.Receive() }()
	putErr := make(chan error, 1)
	go func() { putErr <- vm.LoadSnapshotUFFD(vmStateFile, plan.sockPath, overrides) }()

	select {
	case err := <-recvErr:
		if err != nil {
			_ = h.Close()
			<-putErr // let the load unwind so the FC process is reaped by the caller
			return nil, fmt.Errorf("child uffd handshake: %w", err)
		}
	case err := <-putErr:
		// The load returned before the handshake. A SUCCESSFUL load blocks until Serve
		// fills its restore faults, so reaching here means the load failed (or exited
		// early); tear down so Receive's Accept unblocks, then fail closed.
		_ = h.Close()
		<-recvErr
		if err != nil {
			return nil, fmt.Errorf("load snapshot (child uffd): %w", err)
		}
		return nil, fmt.Errorf("load snapshot (child uffd) returned before the uffd handshake")
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Serve() }()
	select {
	case err := <-putErr:
		if err != nil {
			_ = h.Close()
			<-serveErr
			return nil, fmt.Errorf("load snapshot (child uffd): %w", err)
		}
		// Load complete; Serve keeps running for the life of the child (its final error
		// is drained via the buffered channel at teardown, never blocking).
		return h, nil
	case err := <-serveErr:
		// Serve ended while the load was still faulting: the restore is wedged on a page
		// the handler can no longer fill. Fail closed rather than hang.
		_ = h.Close()
		<-putErr
		if err != nil {
			return nil, fmt.Errorf("child uffd serve ended during restore: %w", err)
		}
		return nil, fmt.Errorf("child uffd serve ended before the restore completed")
	}
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
	// Refuse the RESERVED pre-warm slot id: checkVMID only enforces the character
	// allowlist, so a caller could otherwise pass the internal slot id and collide
	// with the pod's pre-warmed dormant child (adopting or tearing down the slot's
	// Firecracker out from under the pool). The reserved id is engine-internal and
	// is never a tenant fork, so it is rejected up front.
	if id == prewarmSlotVMID {
		return SpawnVMResult{
			OK:    false,
			VMID:  req.VMID,
			Error: fmt.Sprintf("husk: spawn-vm refused: vm id %q is reserved for the pre-warm slot", req.VMID),
		}
	}
	// A CO-LOCATED fork child clones its rootfs from the FROZEN source rootfs the
	// fork snapshot carries at SnapshotDir/rootfs.ext4 (inherit the parent's DISK),
	// NOT the pool template rootfs. Empty otherwise, so a non-fork spawn keeps the
	// template clone. This mirrors the new-pod fork child, whose --rootfs points at
	// the same frozen source rootfs (huskForkRootfsInPodPath).
	rootfsSrcOverride := ""
	if req.Activate.ForkSnapshot && req.Activate.SnapshotDir != "" {
		rootfsSrcOverride = filepath.Join(req.Activate.SnapshotDir, "rootfs.ext4")
	}
	// When this pod has an armed parent-side live-cow WP handler (SetLiveCowParent)
	// AND --live-cow-fork is on, the co-located child imports its guest RAM from the
	// parent's LIVE shared memfd (composed per page with the FROZEN overlay) instead
	// of restoring the memory image from the disk fork snapshot. It does so LAZILY,
	// through Firecracker's NATIVE Uffd restore backend: activateInstance points
	// /snapshot/load at a husk-side handler socket, so the child faults only its
	// working set in on demand rather than eagerly copying all 256MiB (the
	// fork-latency fix, childuffd.go). No FIRECRACKER_MITOS_CHILD_MEMFD env and no
	// disk mem file are needed on this path. FAIL-CLOSED: any error assembling the
	// import logs and leaves childUFFDPlan nil, so activateInstance restores from disk
	// (which is present unless the fork went vmstate-only); turning the flag on never
	// breaks a fork.
	var childPlan *lazyChildUFFDPlan
	if s.liveCowForkApplies(req.Activate) && s.liveCowChildImport {
		plan, err := s.liveCowChildUFFDPlan(id, req.Activate)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "husk: live-cow child uffd import unavailable for vm %q (%v); restoring from disk fork snapshot this pass\n", req.VMID, err)
		case plan != nil:
			childPlan = plan
			fmt.Fprintf(os.Stderr, "husk: live-cow child vm %q lazily faults its guest RAM from the shared parent memfd (uffd backend %s); no disk mem file needed\n", req.VMID, plan.sockPath)
		default:
			fmt.Fprintf(os.Stderr, "husk: live-cow fork enabled for co-located child vm %q but no armed live-cow parent; restoring from disk fork snapshot this pass\n", req.VMID)
		}
	}
	// Per-stage timing of the whole spawn (prepare + activate), so the controller
	// attributes WHAT dominates a co-located fork child: fc_boot / rootfs_clone
	// come from prepare, vmstate_restore / guest_ready / handshake from activate.
	prepStart := time.Now()
	prepStages := make(map[string]float64, 3)
	// Consume a pre-warmed dormant child when one is ready: ADOPT its already-booted
	// generic Firecracker into this fork's instance so the process boot (fc_boot) and
	// the template snapshot verify are OFF the fork hot path (paid ahead), leaving
	// only this fork's rootfs clone to run here. A miss (pre-warm off, or the slot not
	// yet warm) falls back to the on-demand prepare, byte-for-byte the prior behavior.
	// The pre-warmed child is GENERIC (no fork-specific launch env): the live-cow
	// lazy-UFFD import (childPlan) is armed on the dormant instance AFTER prepare and
	// BEFORE activate below, exactly as on the on-demand path, so the pre-warm never
	// changes HOW the child restores (disk mem, or the husk UFFD fault handler when
	// armed). The pre-warmed child counts against the pod's per-VM memory budget, so
	// at most one is kept.
	var prepErr error
	if prewarmed := s.consumePrewarmedChild(); prewarmed != nil {
		if err := s.prepareInstanceOpt(ctx, id, prepareOpts{
			rootfsSrcOverride: rootfsSrcOverride,
			reuseVM:           prewarmed,
			stages:            prepStages,
		}); err != nil {
			prepErr = fmt.Errorf("adopt pre-warmed child: %w", err)
		}
	} else {
		prepErr = s.prepareInstance(ctx, id, rootfsSrcOverride, prepStages)
	}
	// Replenish the pool for the NEXT fork, OFF this fork's hot path, whether we hit
	// or missed the slot (a miss warms it so a later fork skips the boot). A no-op
	// when pre-warm is off or a re-warm is already in flight, so the fork never waits
	// on it and the pod never keeps more than one dormant child.
	s.rewarmPrewarmChildAsync()
	if prepErr != nil {
		return SpawnVMResult{
			OK:    false,
			VMID:  req.VMID,
			Error: fmt.Errorf("husk: spawn-vm prepare vm %q: %w", req.VMID, prepErr).Error(),
		}
	}
	prepStages["prepare_total"] = stageMs(prepStart)
	// Arm the dormant instance with the lazy-UFFD import plan (if any) so
	// activateInstance takes the Uffd backend at its load step. Set under the
	// instance lock, before activate re-acquires it.
	if childPlan != nil {
		s.armInstanceChildUFFD(id, childPlan)
	}
	res, _ := s.activateInstance(ctx, id, req.Activate)
	// Merge the prepare and activate stage maps into one breakdown for the spawn.
	// prepare and activate use disjoint stage names, so neither overwrites the
	// other; the merged map is what the controller logs and observes.
	stages := prepStages
	for name, dur := range res.Stages {
		stages[name] = dur
	}
	return SpawnVMResult{
		OK:            res.OK,
		VMID:          req.VMID,
		VsockPath:     res.VsockPath,
		LatencyMs:     res.LatencyMs,
		Error:         res.Error,
		AlreadyActive: res.AlreadyActive,
		Stages:        stages,
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
	// Tear down the lazy-UFFD child import handler AFTER the VMM is gone: closing the
	// child Firecracker stops it faulting, so the handler's Serve loop then unblocks
	// cleanly and its source-view munmaps race nothing. A no-op on the disk-restore
	// path (nil handler). Also clear any unconsumed plan so a re-prepare starts clean.
	inst.childUFFDPlan = nil
	if inst.childUFFD != nil {
		_ = inst.childUFFD.Close()
		inst.childUFFD = nil
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

	// Tear down the armed source-side live-cow WP handler (m6b) so a stuck Receive
	// and the Serve fault loop stop with the pod. A no-op when the source was never
	// armed. Done alongside the VMM closes so the whole live-cow apparatus goes down
	// with the pod.
	s.closeLiveCowSource()

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
	// Per-stage timing of the paused checkpoint window, so the controller sees
	// whether the fork-snapshot cost is the CreateSnapshot memory write, the
	// live-cow freeze, or the rootfs freeze. Timing/observability only, under the
	// instance lock.
	stages := make(map[string]float64, 5)
	mark := time.Now()

	if err := inst.vm.Pause(); err != nil {
		werr := fmt.Errorf("husk: pause source vm %q for fork snapshot: %w", id, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}
	recordStage(stages, "pause", mark)
	mark = time.Now()

	// Live-cow fork capture (issue #832): when the live-cow path is armed (flag on
	// AND a parent-side WP handle that can freeze the running source), FREEZE the
	// source guest memfd (UFFD write-protect the whole live region, ~microseconds)
	// and capture ONLY the small device/CPU vmstate, INSTEAD OF writing the whole
	// guest RAM to a `mem` file (the ~364ms Full-snapshot cost). The co-located
	// child boots its guest RAM from the parent's shared memfd (m5), so the disk
	// `mem` file is redundant on this path; the freeze keeps a resumed source from
	// leaking a post-fork write forward into the child (the m2 no-leak invariant,
	// docs/fork-correctness.md). On any failure the source is resumed (never leave a
	// tenant's live sandbox frozen) before failing closed, exactly as the Full path.
	//
	// FALLBACK (flag off, or no armed parent, or a non-live-cow pod): the Full
	// CreateSnapshot(mem, vmstate) runs byte-for-byte as before, so a fork never
	// breaks. This selector is the ONLY behavior change; off is the current path.
	//
	// CHILD-RESTORABILITY GATE (the prod hang fix): the vmstate-only capture writes
	// NO `mem` file on the PROMISE that the co-located child boots its guest RAM
	// from the source's shared memfd. That promise holds ONLY when a child-side
	// memfd-import Firecracker patch is shipped (s.liveCowChildImport). The shipped
	// patched binary patches the SOURCE (restore) side ONLY, so a co-located child
	// RESTORES FROM THE DISK fork snapshot: skipping the `mem` file then leaves it
	// with nothing to restore and the fork HANGS (children stuck Restoring, the
	// v1.32.2 prod canary). So an ARMED source still takes the Full path (writes
	// `mem`) unless child import is enabled; the freeze is only worthwhile when the
	// child actually consumes the frozen memfd. This keeps a re-enabled live-cow
	// source's forks fast and RESTORABLE (the proven disk path) instead of hung.
	// SPILL GATE (see ForkSnapshotRequest.RequireMemFile): child import only makes the
	// mem-skip safe for a child CO-LOCATED in this pod, which is the only child that
	// can reach this source's shared memfd. When the controller tells us a child will
	// land in its OWN pod, that child can restore only from disk, so the Full path must
	// write the mem file or the fork hangs with the spilled children stuck Restoring.
	if freezer := s.liveCowSnapshotFreezer(); freezer != nil && s.liveCowChildImport && !req.RequireMemFile {
		if _, err := freezer.Freeze(); err != nil {
			_ = s.resumeInstanceAfterFork(id, inst)
			werr := fmt.Errorf("husk: freeze source guest for live-cow fork snapshot vm %q: %w", id, err)
			return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
		}
		recordStage(stages, "freeze", mark)
		mark = time.Now()
		if err := inst.vm.CreateSnapshotVMStateOnly(vmStateFile); err != nil {
			_ = s.resumeInstanceAfterFork(id, inst)
			werr := fmt.Errorf("husk: create vmstate-only fork snapshot in %s for vm %q: %w", req.SnapshotDir, id, err)
			return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
		}
		recordStage(stages, "create_snapshot", mark)
	} else {
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
		recordStage(stages, "create_snapshot", mark)
	}
	mark = time.Now()

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
	recordStage(stages, "rootfs_freeze", mark)
	mark = time.Now()

	// ALWAYS resume the source after the checkpoint: the pause is only the brief
	// quiescence CreateSnapshot requires, and the memory + frozen rootfs are a
	// consistent point-in-time pair, so the source is safe to run again. Leaving it
	// paused was the v1.24.1 production bug. The resume is retried a few times so a
	// transient blip does not recreate that stuck-paused incident.
	if err := s.resumeInstanceAfterFork(id, inst); err != nil {
		werr := fmt.Errorf("husk: resume source vm %q after fork snapshot: %w", id, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}
	recordStage(stages, "resume", mark)

	latency := time.Since(start)
	fmt.Fprintf(os.Stderr, "husk: fork-snapshot vm %q stage timing ms: %s total=%.2f\n", id, formatStages(stages), float64(latency.Microseconds())/1000.0)
	return ForkSnapshotResult{
		OK:          true,
		SnapshotDir: req.SnapshotDir,
		LatencyMs:   float64(latency.Microseconds()) / 1000.0,
		Stages:      stages,
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
