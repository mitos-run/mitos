//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"mitos.run/mitos/internal/guestenv"
	"mitos.run/mitos/internal/vsock"
)

// streamChunkBytes bounds one stdout/stderr read before it is framed. 32 KiB
// keeps a frame small relative to vsock.MaxMessageBytes and flushes output
// promptly to the host.
const streamChunkBytes = 32 << 10

// execStreamSink receives the output of a streaming exec. The same spawn and
// stream engine (runExecStream) drives both the legacy JSON-lines path and the
// gRPC sandbox.v1.Sandbox.Exec path; only the sink differs, so the security
// invariants (env merge, process group kill, exit-code mapping, no secret
// logging) live in exactly one place. The sink methods are called from the two
// pump goroutines and the wait path; runExecStream serializes them behind a
// single mutex so chunk and exit emissions never interleave.
type execStreamSink interface {
	// chunk emits one stdout or stderr slice. The bytes are owned by the
	// callee once returned (runExecStream passes a fresh copy).
	chunk(stream vsock.StreamName, data []byte)
	// exit emits the terminal exit. spawnErr is a non-secret remediation
	// string set only when the process never started; it is empty otherwise.
	exit(exitCode int, execTimeMs float64, spawnErr string)
}

// handleExecStream runs req.Command and writes ExecStreamFrame lines (chunk
// frames per stream, then one exit frame) directly to conn. It is invoked on a
// DEDICATED connection: the whole reply is this stream, so writing many lines
// is safe. The command runs in its own process group so a context cancel
// (connection drop) kills the whole tree.
func handleExecStream(conn net.Conn, req *vsock.ExecRequest) {
	runExecStream(context.Background(), req, &frameSink{conn: conn})
}

// frameSink is the legacy JSON-lines sink: it marshals each emission to a
// newline-delimited ExecStreamFrame on conn, preserving the exact wire shape
// the host's RunExecStream and /v1/exec/stream clients already parse.
type frameSink struct{ conn net.Conn }

func (s *frameSink) chunk(stream vsock.StreamName, data []byte) {
	writeFrame(s.conn, vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: stream, Data: data})
}

func (s *frameSink) exit(exitCode int, execTimeMs float64, spawnErr string) {
	writeFrame(s.conn, vsock.ExecStreamFrame{
		Kind:       vsock.FrameExit,
		ExitCode:   exitCode,
		ExecTimeMs: execTimeMs,
		Error:      spawnErr,
	})
}

// runExecStream is the shared exec spawn+stream engine. It spawns req.Command
// under /bin/sh with the merged sandbox environment, in its own process group
// so a timeout or a cancelled parent ctx kills the whole tree, and drives sink
// with stdout/stderr chunks then exactly one terminal exit. It never logs the
// command, env values, or output: secret values that flow through req.Env or
// the configured environment never reach a log. The parent ctx lets a gRPC
// stream cancel propagate (client hang-up) on top of the per-exec timeout.
func runExecStream(parent context.Context, req *vsock.ExecRequest, sink execStreamSink) {
	start := time.Now()

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}
	// New process group so cancel/timeout kills children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the whole group (negative pid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = guestenv.Merge(os.Environ(), configured, req.Env)

	// One mutex serializes every sink call so stdout, stderr, and the exit
	// emission never interleave (on the wire, or as gRPC stream sends).
	var sinkMu sync.Mutex
	emitExit := func(exitCode int, spawnErr string) {
		sinkMu.Lock()
		sink.exit(exitCode, float64(time.Since(start).Microseconds())/1000.0, spawnErr)
		sinkMu.Unlock()
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		emitExit(1, fmt.Sprintf("stdout pipe: %v", err))
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		emitExit(1, fmt.Sprintf("stderr pipe: %v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		emitExit(1, fmt.Sprintf("start: %v", err))
		return
	}

	var wg sync.WaitGroup
	pump := func(r io.Reader, stream vsock.StreamName) {
		defer wg.Done()
		buf := make([]byte, streamChunkBytes)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sinkMu.Lock()
				sink.chunk(stream, append([]byte(nil), buf[:n]...))
				sinkMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdoutPipe, vsock.StreamStdout)
	go pump(stderrPipe, vsock.StreamStderr)
	wg.Wait()

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		// Check the deadline first: a timed-out command is SIGKILLed by the
		// cancel below, which surfaces as an ExitError with code -1, so the
		// DeadlineExceeded check must win to report the conventional 124.
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	emitExit(exitCode, "")
}

// writeFrame marshals one frame and writes it as a single newline-delimited
// line. A write error means the host hung up; the caller's pumps will end when
// the pipes close.
func writeFrame(conn net.Conn, f vsock.ExecStreamFrame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}
