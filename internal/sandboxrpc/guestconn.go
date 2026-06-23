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
}
