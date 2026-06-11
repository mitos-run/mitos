package husk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/paperclipinc/sandbox/internal/firecracker"
	"github.com/paperclipinc/sandbox/internal/vsock"
)

// State is the husk stub lifecycle state.
type State int

const (
	// StateNew is before Prepare: no VMM exists.
	StateNew State = iota
	// StateDormant is after Prepare: the Firecracker process and its API
	// socket are up but no snapshot is loaded and the guest is not running.
	StateDormant
	// StateActive is after a successful Activate: the snapshot is loaded,
	// the VM is resumed, and the guest agent has answered over vsock.
	StateActive
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateDormant:
		return "dormant"
	case StateActive:
		return "active"
	default:
		return "unknown"
	}
}

// vmm is the subset of *firecracker.Client the stub drives. Keeping it behind an
// interface lets the activate state machine be unit-tested with a fake, with no
// real Firecracker process or KVM.
type vmm interface {
	// LoadSnapshotWithOverrides loads the snapshot mem+vmstate files and (when
	// resume is true) resumes the VM, remapping NICs per overrides.
	LoadSnapshotWithOverrides(mem, snapshot string, resume bool, overrides []firecracker.NetworkOverride) error
	// VsockHostPath resolves a relative vsock uds_path to its host location.
	VsockHostPath(rel string) string
	// Close tears the VMM down.
	Close() error
}

// starter brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and returns it behind the vmm interface. The production starter wraps
// firecracker.StartVM; tests inject a fake.
type starter func(cfg firecracker.VMConfig) (vmm, error)

// guestReady blocks until the guest agent answers a ping over the vsock UDS at
// vsockPath, or the timeout elapses. The production seam connects via
// internal/vsock and pings; tests inject a fake.
type guestReady func(vsockPath string, timeout time.Duration) error

// productionStarter wraps firecracker.StartVM. *firecracker.Client satisfies
// vmm (it has LoadSnapshotWithOverrides, VsockHostPath, and we adapt Kill to
// Close below).
func productionStarter(cfg firecracker.VMConfig) (vmm, error) {
	client, err := firecracker.StartVM(cfg)
	if err != nil {
		return nil, err
	}
	return &clientVMM{Client: client}, nil
}

// clientVMM adapts *firecracker.Client to the vmm interface. Close maps to Kill
// so the husk teardown reaps the Firecracker process.
type clientVMM struct {
	*firecracker.Client
}

func (c *clientVMM) Close() error {
	return c.Client.Kill()
}

// productionGuestReady retries a vsock connect + ping until the guest answers or
// the timeout elapses. It mirrors how cmd/bench waits for a restored guest: the
// agent listens on vsock.AgentPort and answers Ping once the VM is resumed.
func productionGuestReady(vsockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := vsock.Connect(vsockPath, vsock.AgentPort)
		if err == nil {
			_, perr := client.Ping()
			client.Close()
			if perr == nil {
				return nil
			}
			lastErr = perr
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("guest agent not ready within %s: %w", timeout, lastErr)
}

// Options configures a Stub. Zero values select the production seams, so the
// daemon constructs New(cfg, Options{}). Tests inject fakes.
type Options struct {
	// Start brings up the dormant VMM. Nil uses the production starter.
	Start starter
	// Ready waits for the guest agent. Nil uses the production seam.
	Ready guestReady
	// ReadyTimeout bounds the guest-readiness wait during Activate. Zero uses
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
}

// DefaultReadyTimeout bounds how long Activate waits for the guest agent to
// answer after the snapshot is resumed before failing closed.
const DefaultReadyTimeout = 10 * time.Second

// Stub is a single-VM husk: Prepare brings up a dormant VMM, Activate loads a
// snapshot into it in place, and Serve dispatches one activate request from a
// control socket. It owns exactly one VM for its lifetime.
type Stub struct {
	start        starter
	ready        guestReady
	cfg          firecracker.VMConfig
	readyTimeout time.Duration

	mu    sync.Mutex
	state State
	vm    vmm
}

// New builds a Stub for the given VMConfig. By default it uses the production
// starter and guest-readiness seam; opts may inject fakes for tests.
func New(cfg firecracker.VMConfig, opts Options) *Stub {
	s := &Stub{
		start:        opts.Start,
		ready:        opts.Ready,
		cfg:          cfg,
		readyTimeout: opts.ReadyTimeout,
		state:        StateNew,
	}
	if s.start == nil {
		s.start = productionStarter
	}
	if s.ready == nil {
		s.ready = productionGuestReady
	}
	if s.readyTimeout == 0 {
		s.readyTimeout = DefaultReadyTimeout
	}
	return s
}

// Prepare brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and stores it. It is not idempotent across states: calling it once
// the stub is already dormant or active is an error, so a husk never silently
// leaks a second VMM.
func (s *Stub) Prepare(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateNew {
		return fmt.Errorf("husk: prepare in state %s: already prepared", s.state)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, err := s.start(s.cfg)
	if err != nil {
		return fmt.Errorf("husk: prepare dormant VMM: %w", err)
	}
	s.vm = vm
	s.state = StateDormant
	return nil
}

// Activate loads the snapshot into the dormant VMM in place and waits for the
// guest agent to answer.
//
// It FAILS CLOSED: the stub must be dormant (else error and no result), and any
// snapshot-load or guest-readiness failure returns OK=false plus an error and
// leaves the stub NOT active. A failed Activate never reports a usable VM; the
// caller must treat the husk as unusable.
func (s *Stub) Activate(ctx context.Context, req ActivateRequest) (ActivateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateDormant {
		return ActivateResult{OK: false, Error: fmt.Sprintf("activate in state %s: must be dormant", s.state)},
			fmt.Errorf("husk: activate in state %s: must be dormant", s.state)
	}
	if err := ctx.Err(); err != nil {
		return ActivateResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ActivateResult{OK: false, Error: "activate: empty snapshot dir"},
			fmt.Errorf("husk: activate: empty snapshot dir")
	}

	// Same snapshot file layout the fork engine writes: SnapshotDir/mem and
	// SnapshotDir/vmstate.
	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")

	start := time.Now()
	if err := s.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, true, req.NetworkOverrides); err != nil {
		// Fail closed: the snapshot did not load; the VM is not usable. Leave
		// state dormant so a retry (or teardown) can decide what to do.
		werr := fmt.Errorf("husk: load snapshot from %s: %w", req.SnapshotDir, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	vsockPath := s.vm.VsockHostPath(firecracker.VsockRelPath)
	if err := s.ready(vsockPath, s.readyTimeout); err != nil {
		// Fail closed: the snapshot loaded but the guest never answered, so we
		// cannot vouch for the VM. Do NOT mark active or report a usable VM.
		werr := fmt.Errorf("husk: guest not ready after activate: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	latency := time.Since(start)
	s.state = StateActive
	return ActivateResult{
		OK:        true,
		VsockPath: vsockPath,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// Serve accepts control connections on ln and dispatches each to Activate,
// replying with the ActivateResult. A husk owns one VM, so once an Activate
// SUCCEEDS Serve stops accepting new connections and returns nil: the VM is
// live and there is nothing more to activate. Before a successful activate it
// keeps serving (so a failed-closed activate can be retried) until ctx is
// cancelled or the listener closes. Per-connection errors are returned to the
// peer in the result and do not tear down the server.
func (s *Stub) Serve(ctx context.Context, ln net.Listener) error {
	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("husk: accept control connection: %w", err)
		}
		activated := s.handleConn(ctx, conn)
		if activated {
			// One VM per husk: the activate succeeded, so stop listening.
			return nil
		}
	}
}

// handleConn reads one ActivateRequest, runs Activate, writes the result, and
// reports whether the activate succeeded. Connection-level read/write failures
// are logged to stderr (paths only, no secrets) and do not propagate.
func (s *Stub) handleConn(ctx context.Context, conn net.Conn) (activated bool) {
	defer conn.Close()
	req, err := ReadRequest(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "husk: read activate request: %v\n", err)
		return false
	}
	res, _ := s.Activate(ctx, req)
	if werr := WriteResult(conn, res); werr != nil {
		fmt.Fprintf(os.Stderr, "husk: write activate result: %v\n", werr)
		// The result may not have reached the peer, but the VM state is what it
		// is; report activation per the result we computed.
	}
	return res.OK
}

// State returns the current lifecycle state.
func (s *Stub) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Close tears down the VMM if one was prepared. It is safe to call in any state.
func (s *Stub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return nil
	}
	err := s.vm.Close()
	s.vm = nil
	s.state = StateNew
	return err
}
