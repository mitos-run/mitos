package vsock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// DefaultRequestTimeout bounds a single one-shot host->guest request (write the
// line, read the response line). The guest is in-process on the same host over a
// local vsock/unix socket, so a healthy response is sub-second; this generous
// ceiling still bounds a malicious or wedged guest agent that connects then
// stalls (or dribbles a partial line under the MaxMessageBytes cap) so it cannot
// hang the host caller goroutine, vsock fd, and (for the husk stub) stream slot
// indefinitely. It is overridable per Client via SetRequestTimeout for any
// legitimately slow large response. The long-lived STREAMING paths (ExecStream,
// RunCode, Pty) are NOT bounded by this: they cancel via ctx/conn.Close instead,
// which still bounds a stall by the caller's context.
const DefaultRequestTimeout = 60 * time.Second

// Client communicates with the guest agent over vsock (or Unix socket for testing).
type Client struct {
	conn    net.Conn
	scanner *bufio.Scanner
	// requestTimeout is the per-request read deadline applied in send. Zero
	// selects DefaultRequestTimeout; a negative value disables the deadline.
	requestTimeout time.Duration
}

func newClient(conn net.Conn) *Client {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	return &Client{conn: conn, scanner: scanner}
}

// SetRequestTimeout overrides the per-request read deadline send applies. A zero
// duration restores DefaultRequestTimeout; a negative duration disables the
// deadline (for a caller that genuinely needs an unbounded one-shot read).
func (c *Client) SetRequestTimeout(d time.Duration) {
	c.requestTimeout = d
}

// readTimeout returns the effective per-request read deadline.
func (c *Client) readTimeout() time.Duration {
	if c.requestTimeout == 0 {
		return DefaultRequestTimeout
	}
	return c.requestTimeout
}

// Connect to a guest agent via the Firecracker vsock UDS path.
// Firecracker exposes vsock as a Unix socket on the host.
func Connect(udsPath string, guestPort int) (*Client, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to vsock UDS: %w", err)
	}

	// Firecracker vsock UDS protocol: send "CONNECT <port>\n", expect "OK <port>\n"
	connectCmd := fmt.Sprintf("CONNECT %d\n", guestPort)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	// Bound the preamble read so a host that opened the UDS but never gets the
	// "OK <port>" line back (a wedged Firecracker vsock mux or a stalling guest)
	// does not block forever.
	_ = conn.SetReadDeadline(time.Now().Add(DefaultRequestTimeout))
	if scanner.Scan() {
		resp := scanner.Text()
		if len(resp) < 2 || resp[:2] != "OK" {
			conn.Close()
			return nil, fmt.Errorf("vsock CONNECT rejected: %s", resp)
		}
	} else {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: no response")
	}
	_ = conn.SetReadDeadline(time.Time{})

	return &Client{conn: conn, scanner: scanner}, nil
}

// ConnectUnix connects via Unix socket (for local testing without KVM).
func ConnectUnix(sockPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect unix: %w", err)
	}
	return newClient(conn), nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) send(req *Request) (*Response, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	// Bound the response read so a stalled or dribbling guest cannot hang this
	// caller goroutine forever. The deadline covers the whole one-shot read; it
	// is cleared after so it never leaks onto a later use of the same conn. A
	// negative timeout disables the bound for a caller that opted out.
	if d := c.readTimeout(); d > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}
		defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	}

	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		return nil, fmt.Errorf("connection closed")
	}

	var resp Response
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if !resp.OK {
		return &resp, fmt.Errorf("agent error: %s", resp.Error)
	}
	return &resp, nil
}

func (c *Client) Ping() (float64, error) {
	resp, err := c.send(&Request{Type: TypePing})
	if err != nil {
		return 0, err
	}
	return resp.Ping.Uptime, nil
}

// Vitals asks the guest agent for a one-shot telemetry snapshot (CPU steal,
// memory vs balloon, and the in-guest process table). It is the host half of the
// Layer 3 guest telemetry bridge. The guest samples /proc on receipt, so this
// call blocks for the guest's short sampling window plus round-trip. The reply
// carries no secrets: process entries are program names and resource counters,
// never argv or environment.
func (c *Client) Vitals() (*VitalsResponse, error) {
	resp, err := c.send(&Request{Type: TypeVitals, Vitals: &VitalsRequest{}})
	if err != nil {
		return nil, err
	}
	if resp.Vitals == nil {
		return nil, fmt.Errorf("vitals: empty response")
	}
	return resp.Vitals, nil
}

func (c *Client) Exec(command string, workingDir string, env map[string]string, timeout int) (*ExecResponse, error) {
	resp, err := c.send(&Request{
		Type: TypeExec,
		Exec: &ExecRequest{
			Command:    command,
			WorkingDir: workingDir,
			Env:        env,
			Timeout:    timeout,
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.Exec, nil
}

// Configure delivers claim-time env and secrets to the guest agent.
func (c *Client) Configure(env, secrets map[string]string) error {
	_, err := c.send(&Request{
		Type:      TypeConfigure,
		Configure: &ConfigureRequest{Env: env, Secrets: secrets},
	})
	return err
}

// NotifyForked tells the guest agent a restore just happened so it can reseed
// the kernel CRNG, step the wall clock, and signal userspace runtimes.
// HostWallClockNanos is stamped at send time so the guest measures drift
// against the moment of delivery. Entropy is sensitive seed material and is
// never logged.
func (c *Client) NotifyForked(generation uint64, entropy []byte) (*NotifyForkedResponse, error) {
	return c.NotifyForkedWithNetwork(generation, entropy, nil)
}

// NotifyForkedWithNetwork is NotifyForked plus an optional per-fork network
// config the guest applies to eth0 (distinct guest IP + gateway). It is used
// when host-side networking is enabled so each fork, which restores the same
// snapshot-baked guest IP, is re-addressed to its allocator-assigned /30.
// Passing nil network is identical to NotifyForked. The IPs are safe to log.
func (c *Client) NotifyForkedWithNetwork(generation uint64, entropy []byte, network *NotifyForkedNetwork) (*NotifyForkedResponse, error) {
	return c.NotifyForkedWithConfig(generation, entropy, network, nil)
}

// NotifyForkedWithConfig is NotifyForkedWithNetwork plus the per-fork volume
// mount table the guest mounts after the restore. The host must have already
// rebound each baked placeholder drive to this fork's backing (PATCH /drives)
// before this call, so the devices are in place when the guest mounts them.
// Passing nil volumes is identical to NotifyForkedWithNetwork. Device nodes and
// paths are safe to log.
func (c *Client) NotifyForkedWithConfig(generation uint64, entropy []byte, network *NotifyForkedNetwork, volumes []VolumeMountEntry) (*NotifyForkedResponse, error) {
	resp, err := c.send(&Request{
		Type: TypeNotifyForked,
		NotifyForked: &NotifyForkedRequest{
			Generation:         generation,
			HostWallClockNanos: time.Now().UnixNano(),
			Entropy:            entropy,
			Network:            network,
			Volumes:            volumes,
		},
	})
	if err != nil {
		return nil, err
	}
	return resp.NotifyForked, nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	resp, err := c.send(&Request{
		Type:     TypeReadFile,
		ReadFile: &ReadFileRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	return resp.ReadFile.Content, nil
}

func (c *Client) WriteFile(path string, content []byte, mode uint32) error {
	_, err := c.send(&Request{
		Type:      TypeWriteFile,
		WriteFile: &WriteFileRequest{Path: path, Content: content, Mode: mode},
	})
	return err
}

func (c *Client) ListDir(path string) ([]FileEntry, error) {
	resp, err := c.send(&Request{
		Type:    TypeListDir,
		ListDir: &ListDirRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	return resp.ListDir.Entries, nil
}

func (c *Client) Mkdir(path string) error {
	_, err := c.send(&Request{Type: TypeMkdir, Mkdir: &MkdirRequest{Path: path}})
	return err
}

func (c *Client) Remove(path string) error {
	_, err := c.send(&Request{Type: TypeRemove, Remove: &RemoveRequest{Path: path}})
	return err
}

// TarDir asks the guest agent to tar the directory at path and returns the tar
// bytes. The guest restricts path to the workspace-transfer allowlist and bounds
// the tar to MaxTarBytes. This is the host half of the bulk workspace dehydrate.
func (c *Client) TarDir(path string) ([]byte, error) {
	resp, err := c.send(&Request{
		Type:   TypeTarDir,
		TarDir: &TarDirRequest{Path: path},
	})
	if err != nil {
		return nil, err
	}
	if resp.TarDir == nil {
		return nil, fmt.Errorf("tar_dir: empty response")
	}
	return resp.TarDir.Tar, nil
}

// UntarDir asks the guest agent to extract tar into the directory at path. The
// caller must keep tar within MaxTarBytes (this is the size the guest accepts and
// the line buffer holds). The guest sanitizes every member against traversal.
// This is the host half of the bulk workspace hydrate.
func (c *Client) UntarDir(path string, tar []byte) error {
	if len(tar) > MaxTarBytes {
		return fmt.Errorf("untar_dir: tar size %d exceeds max %d", len(tar), MaxTarBytes)
	}
	_, err := c.send(&Request{
		Type:     TypeUntarDir,
		UntarDir: &UntarDirRequest{Path: path, Tar: tar},
	})
	return err
}

// StreamConn is a DEDICATED vsock connection for one streaming exec. It is kept
// separate from Client.conn so a long-running stream never interleaves with the
// shared connection's one-shot Response calls (Ping, file ops, aggregated Exec).
type StreamConn struct {
	conn    net.Conn
	scanner *bufio.Scanner
	// writeMu guards all host->guest writes on a bidirectional PTY stream so
	// concurrent input/resize frames never interleave mid-line on the wire.
	writeMu sync.Mutex
	// ptyReady is closed by Pty once the open request is on the wire. SendInput
	// and Resize block on it so an input/resize frame can never reach the guest
	// before the request line that the guest's first read consumes; without this
	// a caller that fires Resize concurrently with Pty could have the guest mistake
	// the resize frame for the request line. Created lazily under initOnce.
	initOnce  sync.Once
	readyOnce sync.Once
	ptyReady  chan struct{}
}

// ptyReadyCh lazily creates and returns the ptyReady channel so both Pty and the
// frame writers observe the same instance.
func (s *StreamConn) ptyReadyCh() chan struct{} {
	s.initOnce.Do(func() {
		s.ptyReady = make(chan struct{})
	})
	return s.ptyReady
}

// signalPtyReady marks the open request as written so blocked input/resize
// writers may proceed. Idempotent.
func (s *StreamConn) signalPtyReady() {
	ch := s.ptyReadyCh()
	s.readyOnce.Do(func() { close(ch) })
}

// DialStream opens a fresh vsock connection to the guest agent for one
// streaming exec, performing the Firecracker UDS CONNECT preamble. The caller
// must Close it when the stream ends or its HTTP client disconnects.
func DialStream(udsPath string, guestPort int) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", udsPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream vsock UDS: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", guestPort))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	// Bound only the CONNECT preamble read; the streaming reads that follow are
	// long-lived and cancel via ctx/conn.Close, so clear the deadline after the
	// preamble succeeds.
	_ = conn.SetReadDeadline(time.Now().Add(DefaultRequestTimeout))
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT: no response")
	}
	if resp := sc.Text(); len(resp) < 2 || resp[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %s", resp)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// DialStreamUnix dials a plain unix socket that already speaks the CONNECT
// preamble (the standalone server's unix fallback and tests).
func DialStreamUnix(sockPath string) (*StreamConn, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stream unix: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", AgentPort))); err != nil {
		conn.Close()
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1024*1024), MaxMessageBytes)
	// Bound only the preamble read; streaming reads that follow cancel via
	// ctx/conn.Close, so clear the deadline once the preamble is in.
	_ = conn.SetReadDeadline(time.Now().Add(DefaultRequestTimeout))
	if !sc.Scan() {
		conn.Close()
		return nil, fmt.Errorf("stream unix: no preamble response")
	}
	_ = conn.SetReadDeadline(time.Time{})
	return &StreamConn{conn: conn, scanner: sc}, nil
}

// Close shuts the dedicated stream connection. Closing it while the guest is
// still running cancels the guest exec (the guest sees the connection drop).
func (s *StreamConn) Close() error {
	return s.conn.Close()
}

// ChunkFunc receives one stream's bytes as they arrive. Returning a non-nil
// error stops the stream early (the caller should then Close the StreamConn).
type ChunkFunc func(stream StreamName, data []byte) error

// ExecStream runs command on the guest and invokes onChunk for each stdout or
// stderr chunk as it arrives, returning the terminal ExecStreamFrame (exit
// code, exec time, and any spawn error). The request is sent once; frames are
// read until the FrameExit line. If ctx is cancelled the connection is closed,
// which the guest observes and uses to kill the process group.
func (s *StreamConn) ExecStream(ctx context.Context, req *ExecRequest, onChunk ChunkFunc) (*ExecStreamFrame, error) {
	data, err := json.Marshal(&Request{Type: TypeExecStream, ExecStream: req})
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send exec_stream: %w", err)
	}

	// Closing the connection on ctx cancel unblocks the scanner below.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f ExecStreamFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("decode exec_stream frame: %w", err)
		}
		switch f.Kind {
		case FrameChunk:
			if err := onChunk(f.Stream, f.Data); err != nil {
				return nil, err
			}
		case FrameExit:
			return &f, nil
		default:
			return nil, fmt.Errorf("unknown exec_stream frame kind: %q", f.Kind)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("recv exec_stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("exec_stream: connection closed before exit frame")
}

// RunCode runs a code snippet in the guest's stateful kernel over this
// dedicated stream and invokes onFrame for each ExecStreamFrame the guest emits
// (chunk frames for stdout/stderr, result frames for rich artifacts, an error
// frame for a structured exception, and a terminal exit). The kernel is started
// lazily by the guest on the first call and persists for the sandbox lifetime,
// so state set by an earlier RunCode is visible here. Returns when the guest
// sends an exit frame; if ctx is cancelled the connection is closed, which the
// guest observes. Code is not logged.
func (s *StreamConn) RunCode(ctx context.Context, req *RunCodeRequest, onFrame func(ExecStreamFrame)) error {
	data, err := json.Marshal(&Request{Type: TypeRunCode, RunCode: req})
	if err != nil {
		return err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("send run_code: %w", err)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f ExecStreamFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return fmt.Errorf("decode run_code frame: %w", err)
		}
		onFrame(f)
		if f.Kind == FrameExit {
			return nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		return fmt.Errorf("recv run_code: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("run_code: connection closed before exit frame")
}

// Exec runs command to completion over the stream and returns the aggregated
// stdout/stderr and exit code, matching the one-shot ExecResponse shape. It is
// the streaming-native equivalent of Client.Exec and is what the HTTP /v1/exec
// handler uses so blocking and streaming share one guest code path.
func (s *StreamConn) Exec(command, workingDir string, env map[string]string, timeout int) (*ExecResponse, error) {
	var out, errb strings.Builder
	exit, err := s.ExecStream(context.Background(), &ExecRequest{
		Command:    command,
		WorkingDir: workingDir,
		Env:        env,
		Timeout:    timeout,
	}, func(stream StreamName, data []byte) error {
		if stream == StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if exit.Error != "" {
		return nil, fmt.Errorf("exec_stream: %s", exit.Error)
	}
	return &ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}, nil
}

// OutputFunc receives one slice of raw PTY output bytes as it arrives.
// Returning a non-nil error stops the stream early.
type OutputFunc func(data []byte) error

// Pty opens an interactive pseudo-terminal in the guest and streams its output
// to onOutput, returning the terminal PtyFrame (exit code, and any spawn
// error). Unlike ExecStream this connection is BIDIRECTIONAL: the caller writes
// input and resize frames concurrently via SendInput and Resize while Pty reads
// output frames. If ctx is cancelled the connection is closed, which the guest
// observes and uses to kill the shell process group.
func (s *StreamConn) Pty(ctx context.Context, req *PtyRequest, onOutput OutputFunc) (*PtyFrame, error) {
	data, err := json.Marshal(&Request{Type: TypePty, Pty: req})
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	_, werr := s.conn.Write(append(data, '\n'))
	s.writeMu.Unlock()
	// Unblock any SendInput/Resize callers now that the open request is on the
	// wire (even on error: a blocked writer should observe the closed conn, not
	// hang forever).
	s.signalPtyReady()
	if werr != nil {
		return nil, fmt.Errorf("send pty: %w", werr)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.conn.Close()
		case <-done:
		}
	}()

	for s.scanner.Scan() {
		var f PtyFrame
		if err := json.Unmarshal(s.scanner.Bytes(), &f); err != nil {
			return nil, fmt.Errorf("decode pty frame: %w", err)
		}
		switch f.Kind {
		case PtyOutput:
			if err := onOutput(f.Data); err != nil {
				return nil, err
			}
		case PtyExit:
			return &f, nil
		default:
			return nil, fmt.Errorf("unexpected pty frame kind: %q", f.Kind)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, fmt.Errorf("recv pty: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("pty: connection closed before exit frame")
}

// TunnelConn is a raw bidirectional byte pipe to a guest loopback TCP port,
// returned by StreamConn.Tunnel once the guest has acked the open. It satisfies
// io.ReadWriteCloser plus a read deadline. Reads first drain any bytes that were
// buffered alongside the ack line (the guest may coalesce early payload with its
// ack in one TCP segment), then read straight from the underlying vsock conn;
// writes and Close go straight to the conn. Closing it drops the vsock
// connection, which the guest observes and uses to close its end of the guest
// TCP socket, so no goroutine or fd leaks on either side.
type TunnelConn struct {
	conn     net.Conn
	leftover []byte
}

// Read drains any leftover post-ack bytes first, then reads from the conn.
func (t *TunnelConn) Read(p []byte) (int, error) {
	if len(t.leftover) > 0 {
		n := copy(p, t.leftover)
		t.leftover = t.leftover[n:]
		return n, nil
	}
	return t.conn.Read(p)
}

func (t *TunnelConn) Write(p []byte) (int, error) { return t.conn.Write(p) }

func (t *TunnelConn) Close() error { return t.conn.Close() }

// SetReadDeadline bounds a read on the tunnel; it forwards to the underlying
// vsock conn. Used by callers (and tests) that must not block forever.
func (t *TunnelConn) SetReadDeadline(d time.Time) error { return t.conn.SetReadDeadline(d) }

// Tunnel opens a raw TCP-over-vsock tunnel to the guest's 127.0.0.1:port (issue
// #228) over this DEDICATED stream connection. It sends one TunnelRequest line,
// reads the guest's single TunnelAck line under a bounded deadline (so a wedged
// guest cannot hang the caller), and on success returns a TunnelConn that is a
// raw bidirectional byte pipe to the guest TCP socket. On a refused open (the
// guest port is not listening, or the target was not loopback) it returns the
// guest's LLM-legible dial error and the caller should Close the StreamConn. The
// returned TunnelConn takes ownership of the connection; Close it to tear the
// tunnel down.
//
// The ack line is read byte-at-a-time directly from the conn (NOT through the
// StreamConn scanner) so the read never over-consumes into a buffer the raw pipe
// cannot reach: every byte after the ack newline stays on the wire for the
// TunnelConn. Any payload that arrived coalesced WITH the ack line is captured
// as leftover and replayed on the first Read.
func (s *StreamConn) Tunnel(port int) (*TunnelConn, error) {
	data, err := json.Marshal(&Request{Type: TypeTunnel, Tunnel: &TunnelRequest{Port: port}})
	if err != nil {
		return nil, err
	}
	if _, err := s.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("send tunnel open: %w", err)
	}

	// Bound only the ack read; the raw byte pipe that follows is long-lived and
	// is torn down by Close, so clear the deadline once the ack is in.
	_ = s.conn.SetReadDeadline(time.Now().Add(DefaultRequestTimeout))
	line, leftover, rerr := readAckLine(s.conn)
	_ = s.conn.SetReadDeadline(time.Time{})
	if rerr != nil {
		return nil, fmt.Errorf("recv tunnel ack: %w", rerr)
	}

	var ack TunnelAck
	if err := json.Unmarshal(line, &ack); err != nil {
		return nil, fmt.Errorf("decode tunnel ack: %w", err)
	}
	if !ack.OK {
		return nil, fmt.Errorf("guest refused tunnel to 127.0.0.1:%d: %s", port, ack.Error)
	}
	return &TunnelConn{conn: s.conn, leftover: leftover}, nil
}

// readAckLine reads bytes from conn until the first newline, returning the line
// WITHOUT the trailing newline and any bytes that arrived in the same read after
// the newline (leftover, which belongs to the raw pipe that follows the ack). It
// reads in small chunks so it cannot over-consume more than one OS read past the
// newline, and that single over-read is preserved as leftover. The ack line is
// bounded by maxAckLineBytes so a guest that never sends a newline cannot drive
// an unbounded host allocation.
func readAckLine(conn net.Conn) (line, leftover []byte, err error) {
	var buf []byte
	tmp := make([]byte, 512)
	for {
		n, rerr := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytesIndexByte(buf, '\n'); i >= 0 {
				return buf[:i], append([]byte(nil), buf[i+1:]...), nil
			}
			if len(buf) > maxAckLineBytes {
				return nil, nil, fmt.Errorf("tunnel ack exceeded %d bytes without a newline", maxAckLineBytes)
			}
		}
		if rerr != nil {
			if len(buf) == 0 {
				return nil, nil, fmt.Errorf("connection closed before ack")
			}
			return nil, nil, fmt.Errorf("connection closed mid-ack")
		}
	}
}

// maxAckLineBytes bounds the host's tunnel-ack read so a guest that connects but
// never sends a newline-terminated ack cannot drive an unbounded allocation.
const maxAckLineBytes = 64 << 10

// bytesIndexByte returns the index of the first b in s, or -1. A tiny local
// helper to avoid importing bytes solely for IndexByte in this file.
func bytesIndexByte(s []byte, b byte) int {
	for i := range s {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// SendInput writes one input frame (raw keystroke bytes) to the guest PTY. Safe
// to call concurrently with Pty; the write is mutex-guarded.
func (s *StreamConn) SendInput(data []byte) error {
	return s.writeFrame(PtyFrame{Kind: PtyInput, Data: data})
}

// Resize writes one resize frame; the guest applies it to the PTY master with
// TIOCSWINSZ, and the kernel delivers SIGWINCH to the foreground group.
func (s *StreamConn) Resize(cols, rows int) error {
	return s.writeFrame(PtyFrame{Kind: PtyResize, Cols: cols, Rows: rows})
}

func (s *StreamConn) writeFrame(f PtyFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	// Wait until Pty has put the open request on the wire so this frame cannot
	// be mistaken for the request by the guest's first read.
	<-s.ptyReadyCh()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write pty frame: %w", err)
	}
	return nil
}
