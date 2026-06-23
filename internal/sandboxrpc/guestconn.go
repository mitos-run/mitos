// Package sandboxrpc: GuestConn is the port (hexagonal architecture seam)
// between the Connect Sandbox service and the in-guest execution surface.
// Each method maps one logical operation; implementations bridge to the real
// guest (vsock JSON-lines today, proto later) or to a scripted fake in tests.
// Tasks 2.2-2.7 add the file, runcode, portforward, vitals, watch/process, and
// error helper methods; only Exec has a working Service method in Task 2.1.
package sandboxrpc

import (
	"context"
	"time"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// RunCodeFrameKind identifies which field of a RunCodeFrame is populated.
type RunCodeFrameKind int

const (
	// RunCodeFrameStdout carries a stdout chunk.
	RunCodeFrameStdout RunCodeFrameKind = iota
	// RunCodeFrameStderr carries a stderr chunk.
	RunCodeFrameStderr
	// RunCodeFrameResult carries rich display output (Jupyter-style).
	RunCodeFrameResult
	// RunCodeFrameError carries a kernel exception.
	RunCodeFrameError
	// RunCodeFrameExit is the terminal frame; ExitCode is valid.
	RunCodeFrameExit
)

// RunCodeFrame is one event from a guest RunCode execution. Kind determines
// which field is populated. When Kind is RunCodeFrameExit, ExitCode is valid
// and the stream is exhausted.
type RunCodeFrame struct {
	// Kind identifies the payload.
	Kind RunCodeFrameKind
	// Stdout carries bytes written to stdout (Kind == RunCodeFrameStdout).
	Stdout []byte
	// Stderr carries bytes written to stderr (Kind == RunCodeFrameStderr).
	Stderr []byte
	// Result carries rich display output (Kind == RunCodeFrameResult).
	// Text is the plain-text representation; Data maps MIME type to payload.
	// No proto or connect types appear here.
	Result *RunCodeResult
	// Error carries a kernel exception (Kind == RunCodeFrameError).
	Error *RunCodeError
	// ExitCode is valid only when Kind == RunCodeFrameExit.
	ExitCode int32
}

// RunCodeResult is the transport-neutral representation of a RunResult frame:
// rich display output from a kernel execution. Text is the plain-text form;
// Data maps MIME type string to raw bytes (e.g. "image/png" to PNG bytes).
// No connect or proto types appear in this struct.
type RunCodeResult struct {
	// Text is the plain-text representation of the output.
	Text string
	// Data maps MIME type to payload bytes (e.g. "text/html" to HTML bytes).
	Data map[string][]byte
}

// RunCodeError is the transport-neutral representation of a RunError frame:
// a kernel exception. No connect or proto types appear in this struct.
type RunCodeError struct {
	// Name is the exception class name (e.g. "ZeroDivisionError").
	Name string
	// Value is the string value of the exception.
	Value string
	// Traceback is the formatted traceback lines.
	Traceback []string
}

// RunCodeStream is the handle returned by GuestConn.RunCode. Recv returns
// successive frames with err == nil for each frame including the terminal
// RunCodeFrameExit frame. After the exit frame, a subsequent call returns
// io.EOF (the Service never makes that call). Other errors indicate a transport
// or guest failure. Close releases resources.
type RunCodeStream interface {
	// Recv returns the next RunCodeFrame with err == nil, including the
	// terminal RunCodeFrameExit frame. io.EOF is returned only on a
	// subsequent call after the exit frame, which the Service never makes.
	// Other errors are transport or guest failures.
	Recv() (*RunCodeFrame, error)
	// Close releases resources. Safe to call after the exit frame.
	Close() error
}

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
// frames (stdout/stderr chunks then the terminal Done frame) with err == nil
// for each frame including the Done frame itself. After the Done frame, a
// subsequent call returns io.EOF (the Service never makes that call). Other
// errors indicate a transport or guest failure. Close releases resources
// regardless of whether all frames were consumed.
type ExecStream interface {
	// Recv returns the next ExecFrame with err == nil, including the terminal
	// Done frame. io.EOF is returned only on a subsequent call after Done,
	// which the Service handler never makes. Other errors are transport
	// or guest failures.
	Recv() (*ExecFrame, error)
	// Close releases resources. Safe to call after the Done frame.
	Close() error
}

// WriteFileResult holds the outcome of a WriteFile guest call.
type WriteFileResult struct {
	// BytesWritten is the total number of bytes written to the file.
	BytesWritten int64
}

// FileInfo mirrors the proto FileInfo fields using Go primitives. It is the
// transport-neutral shape returned by GuestConn.Stat and included in
// GuestConn.List results. No connect or proto types appear here.
type FileInfo struct {
	// Name is the base name of the entry (no path separator).
	Name string
	// Path is the full path inside the sandbox.
	Path string
	// IsDir is true when the entry is a directory.
	IsDir bool
	// Size is the file size in bytes (0 for directories).
	Size int64
	// Mode is the file permission bits (e.g. 0o644).
	Mode uint32
	// ModifiedAtUnix is the mtime in Unix seconds.
	ModifiedAtUnix int64
}

// ListResult holds the outcome of a List guest call including AIP-158
// pagination fields.
type ListResult struct {
	// Entries is the slice of file-info entries for the current page.
	Entries []*FileInfo
	// NextPageToken is non-empty when there are more entries beyond this page.
	// Pass it as page_token in the next List call to retrieve the next page.
	NextPageToken string
}

// GuestConn is the transport-neutral port for in-guest operations. The
// production implementation bridges to the existing vsock/JSON-lines guest
// agent; tests supply a scripted fake. One interface method per logical
// operation keeps each adapter minimal and each test focused.
//
// No connect or proto types appear in this interface; all inputs and outputs
// use Go primitives or local types defined in this file so the seam stays
// clean and testable without a Connect server.
type GuestConn interface {
	// Exec starts a command described by open and returns a stream of output
	// frames (stdout/stderr chunks then a terminal exit). The caller owns the
	// stream and must call Close when done.
	Exec(ctx context.Context, open *sandboxv1.ExecOpen) (ExecStream, error)

	// ReadFile reads the file at path and returns its content as a slice of
	// byte slices (chunks). offset and length allow partial reads; both 0
	// means read the entire file.
	ReadFile(ctx context.Context, path string, offset int64, length int64) ([][]byte, error)

	// WriteFile writes the concatenated chunks to the file at path. mode is
	// the file permission bits (e.g. 0o644); 0 applies a guest default.
	// Returns the total bytes written.
	WriteFile(ctx context.Context, path string, mode uint32, chunks [][]byte) (*WriteFileResult, error)

	// List enumerates the directory at path. pageSize and pageToken implement
	// AIP-158 pagination; filter is an optional glob or expression. Returns
	// the matching entries and a nextPageToken (empty when no more pages).
	List(ctx context.Context, path string, pageSize int32, pageToken string, filter string) (*ListResult, error)

	// Stat returns metadata for the file or directory at path without reading
	// its content.
	Stat(ctx context.Context, path string) (*FileInfo, error)

	// Mkdir creates the directory at path. When recursive is true, all missing
	// parent directories are created (equivalent to mkdir -p).
	Mkdir(ctx context.Context, path string, recursive bool) error

	// Remove deletes the file or directory at path. When recursive is true, a
	// non-empty directory tree is deleted (equivalent to rm -rf).
	Remove(ctx context.Context, path string, recursive bool) error

	// RunCode executes code in the sandbox kernel and returns a stream of
	// output frames (stdout/stderr chunks, result, error, then a terminal
	// exit frame). The caller owns the stream and must call Close when done.
	RunCode(ctx context.Context, open *sandboxv1.RunCodeOpen) (RunCodeStream, error)

	// PortForward opens a bidirectional byte stream to a TCP port inside the
	// sandbox. The caller sends bytes toward the guest via Send and receives
	// bytes from the guest via Recv. The stream ends with a Close frame.
	// The caller owns the stream and must call Close when done.
	PortForward(ctx context.Context, port uint32) (PortForwardStream, error)

	// Vitals returns a server stream of GuestVitals samples emitted at the
	// given interval. The stream runs until the context is cancelled or the
	// guest closes it. The caller owns the stream and must call Close when done.
	Vitals(ctx context.Context, interval time.Duration) (VitalsStream, error)

	// Watch returns a server stream of FsEvents for the subtree at path.
	// The stream runs until the context is cancelled or the guest closes it.
	// The caller owns the stream and must call Close when done.
	Watch(ctx context.Context, path string) (WatchStream, error)

	// Processes returns the current guest process table.
	Processes(ctx context.Context) (*sandboxv1.ProcessList, error)

	// Signal delivers POSIX signal number signal to the process with the given
	// pid inside the sandbox.
	Signal(ctx context.Context, pid int32, signal int32) error
}
