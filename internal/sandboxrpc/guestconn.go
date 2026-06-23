// Package sandboxrpc: GuestConn is the port (hexagonal architecture seam)
// between the Connect Sandbox service and the in-guest execution surface.
// Each method maps one logical operation; implementations bridge to the real
// guest (vsock JSON-lines today, proto later) or to a scripted fake in tests.
// Tasks 2.2-2.7 add the file, runcode, portforward, vitals, watch/process, and
// error helper methods; only Exec has a working Service method in Task 2.1.
package sandboxrpc

import (
	"context"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// ExecFrame is one output event from a guest exec: either a stdout/stderr chunk
// or the terminal exit. When Done is true the ExitCode field is valid and the
// stream is exhausted.
type ExecFrame struct {
	// Stdout carries bytes written to the process stdout. Empty unless this
	// is a stdout chunk (Stderr and Done are both false).
	Stdout []byte
	// Stderr carries bytes written to the process stderr. Empty unless this
	// is a stderr chunk (Stdout and Done are both false).
	Stderr []byte
	// Done is true for the terminal frame. ExitCode is valid only when Done
	// is true.
	Done bool
	// ExitCode is the process exit code, valid only when Done is true.
	ExitCode int32
}

// ExecStream is the handle returned by GuestConn.Exec. Recv returns successive
// frames (stdout/stderr chunks then a terminal Done frame) until io.EOF signals
// the stream is exhausted. Close releases resources regardless of whether all
// frames were consumed.
type ExecStream interface {
	// Recv returns the next ExecFrame. Returns io.EOF after the terminal Done
	// frame has been delivered. Other errors indicate a transport or guest failure.
	Recv() (*ExecFrame, error)
	// Close releases resources. Safe to call after io.EOF.
	Close() error
}

// GuestConn is the transport-neutral port for in-guest operations. The
// production implementation bridges to the existing vsock/JSON-lines guest
// agent; tests supply a scripted fake. One interface method per logical
// operation keeps each adapter minimal and each test focused.
//
// Methods for Tasks 2.2-2.7 (ReadFile, WriteFile, List, Stat, Mkdir, Remove,
// RunCode, PortForward, Vitals, Watch, Processes, Signal) are declared here so
// the interface is complete; only Exec has a working Service method in Task 2.1.
// Each Task 2.x stub adds its real implementation.
type GuestConn interface {
	// Exec starts a command described by open and returns a stream of output
	// frames (stdout/stderr chunks then a terminal exit). The caller owns the
	// stream and must call Close when done.
	Exec(ctx context.Context, open *sandboxv1.ExecOpen) (ExecStream, error)
}
