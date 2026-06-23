//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"mitos.run/mitos/internal/guestenv"
	"mitos.run/mitos/internal/vsock"
)

// ptyOutputChunkBytes bounds one PTY read before it is framed. 32 KiB keeps a
// frame small relative to vsock.MaxMessageBytes and flushes output promptly.
const ptyOutputChunkBytes = 32 << 10

// openPTY opens a new pseudo-terminal pair via /dev/ptmx and returns the master
// file and the slave path (/dev/pts/N). The caller opens the slave on the child
// side. Raw syscalls via golang.org/x/sys/unix keep the guest free of any
// third-party PTY dependency.
func openPTY() (master *os.File, slavePath string, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock the slave (TIOCSPTLCK = 0).
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, "", fmt.Errorf("unlock pts: %w", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, "", fmt.Errorf("get pts number: %w", err)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}

// setWinsize applies cols/rows to the PTY master. The kernel then delivers
// SIGWINCH to the foreground process group automatically.
func setWinsize(master *os.File, cols, rows int) error {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: uint16(cols),
		Row: uint16(rows),
	})
}

// ptyParams is the transport-neutral input to runPTY: the shell command, the
// working dir, the per-call env overlay, and the initial window size. Both the
// JSON-lines handlePtyStream and the gRPC Exec+pty path translate their wire
// shapes into this struct so the PTY spawn, the session-leader SysProcAttr, the
// env merge, and the kill-group logic live in exactly one place.
type ptyParams struct {
	Command    string
	WorkingDir string
	Env        map[string]string
	Cols       int
	Rows       int
}

// ptyInputKind tags a host->guest PTY frame the reader pulled from a transport.
type ptyInputKind int

const (
	ptyInputData   ptyInputKind = iota // stdin bytes to write to the master
	ptyInputResize                     // a window resize (cols/rows)
	ptyInputEOF                        // the host hung up; runPTY kills the shell
)

// ptyTransport is the transport-neutral sink+source runPTY drives. output is
// called with each PTY output chunk (the bytes are owned by the callee), exit
// with the terminal exit code (and a non-secret spawn-failure remediation, set
// only when the shell never started). input blocks reading the next host->guest
// frame and returns its kind: ptyInputData carries data, ptyInputResize carries
// cols/rows, ptyInputEOF means the host hung up and runPTY kills the shell group.
type ptyTransport interface {
	output(data []byte)
	exit(exitCode int, spawnErr string)
	input() (kind ptyInputKind, data []byte, cols, rows int)
}

// runPTY allocates a PTY, starts the shell as a session leader on the slave,
// and pumps PTY<->transport bidirectionally: a reader goroutine pulls
// input/resize frames via t.input() and writes them to the master, the main
// loop frames PTY output back via t.output(), and on shell exit it reports the
// terminal exit via t.exit(). The shell runs in its own session/process group
// so a host hang-up (t.input() returning ok==false) or the host's kill kills
// the whole tree. The env merge is identical to the exec path and TERM is
// exported; secret env values are never logged. This is the shared core behind
// both the JSON-lines PTY stream and the gRPC Exec+pty path.
func runPTY(p ptyParams, t ptyTransport) {
	master, slavePath, err := openPTY()
	if err != nil {
		t.exit(1, err.Error())
		return
	}
	defer master.Close()

	if err := setWinsize(master, p.Cols, p.Rows); err != nil {
		t.exit(1, fmt.Sprintf("set winsize: %v", err))
		return
	}

	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.exit(1, fmt.Sprintf("open slave: %v", err))
		return
	}

	shell := p.Command
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = p.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = "/workspace"
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// New session + controlling terminal on the slave: the shell becomes a
	// session leader in its own process group, so the kernel routes SIGWINCH
	// to it and a group kill reaches every child. Ctty is the child-side fd
	// NUMBER, which os/exec assigns from the order of Stdin/Stdout/Stderr +
	// ExtraFiles: slave is wired to all three standard streams, so it lands on
	// child fd 0. (It is NOT the parent's slave.Fd() raw descriptor; passing
	// that trips "Setctty set but Ctty not valid in child".)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	configuredMu.Lock()
	configured := make(map[string]string, len(configuredEnv))
	for k, v := range configuredEnv {
		configured[k] = v
	}
	configuredMu.Unlock()
	cmd.Env = append(guestenv.Merge(os.Environ(), configured, p.Env), "TERM=xterm-256color")

	if err := cmd.Start(); err != nil {
		slave.Close()
		t.exit(1, fmt.Sprintf("start shell: %v", err))
		return
	}
	// The child holds the slave now; the parent closes its copy so the master
	// sees EOF when the shell exits.
	slave.Close()

	killGroup := func() {
		if cmd.Process != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
	}

	// Reader goroutine: host->guest. Pulls input/resize frames from the
	// transport. ok==false (host hung up or the stream ctx was cancelled) kills
	// the shell group, which makes the master read below return so the main loop
	// and cmd.Wait join: no goroutine leak on client cancel.
	go func() {
		for {
			kind, data, cols, rows := t.input()
			switch kind {
			case ptyInputEOF:
				killGroup()
				return
			case ptyInputResize:
				_ = setWinsize(master, cols, rows)
			case ptyInputData:
				if _, err := master.Write(data); err != nil {
					killGroup()
					return
				}
			}
		}
	}()

	// Main loop: guest->host. Frame PTY output until the master reports EOF
	// (shell exited or all slave fds closed).
	buf := make([]byte, ptyOutputChunkBytes)
	for {
		n, rerr := master.Read(buf)
		if n > 0 {
			t.output(append([]byte(nil), buf[:n]...))
		}
		if rerr != nil {
			break
		}
	}

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	t.exit(exitCode, "")
}

// handlePtyStream allocates a PTY and pumps PTY<->vsock bidirectionally on the
// DEDICATED conn by driving the shared runPTY engine through a JSON-lines
// transport (jsonPtyTransport): a reader goroutine inside runPTY decodes
// input/resize frames from the host, the main loop frames PTY output back, and
// on shell exit it writes the terminal exit frame. The shell runs in its own
// session/process group so a connection drop or the host's kill kills the whole
// tree.
//
// sc is the dispatcher's scanner, handed over (not freshly allocated): it may
// already hold input/resize frames that arrived coalesced with the open-request
// line in a single read (bufio.Scanner reads in chunks). Reusing it ensures
// those early frames are consumed rather than dropped by a fresh scanner.
func handlePtyStream(conn net.Conn, sc *bufio.Scanner, req *vsock.PtyRequest) {
	t := &jsonPtyTransport{conn: conn, sc: sc}
	runPTY(ptyParams{
		Command:    req.Command,
		WorkingDir: req.WorkingDir,
		Env:        req.Env,
		Cols:       req.Cols,
		Rows:       req.Rows,
	}, t)
}

// jsonPtyTransport adapts the shared runPTY engine to the legacy JSON-lines wire
// shape on the dedicated vsock conn. output/exit marshal PtyFrame lines under a
// write mutex (output and exit must not interleave); input scans and decodes the
// next input/resize frame. A scan error (host hung up) returns ok==false so
// runPTY kills the shell group, matching the prior behavior byte-for-byte.
type jsonPtyTransport struct {
	conn    net.Conn
	sc      *bufio.Scanner
	writeMu sync.Mutex
}

func (t *jsonPtyTransport) output(data []byte) {
	t.writeMu.Lock()
	writePtyFrame(t.conn, vsock.PtyFrame{Kind: vsock.PtyOutput, Data: data})
	t.writeMu.Unlock()
}

func (t *jsonPtyTransport) exit(exitCode int, spawnErr string) {
	t.writeMu.Lock()
	writePtyFrame(t.conn, vsock.PtyFrame{Kind: vsock.PtyExit, ExitCode: exitCode, Error: spawnErr})
	t.writeMu.Unlock()
}

func (t *jsonPtyTransport) input() (kind ptyInputKind, data []byte, cols, rows int) {
	for t.sc.Scan() {
		var f vsock.PtyFrame
		if err := json.Unmarshal(t.sc.Bytes(), &f); err != nil {
			continue
		}
		switch f.Kind {
		case vsock.PtyInput:
			return ptyInputData, f.Data, 0, 0
		case vsock.PtyResize:
			return ptyInputResize, nil, f.Cols, f.Rows
		}
	}
	return ptyInputEOF, nil, 0, 0
}

// writePtyFrame marshals one frame and writes it as a single newline-delimited
// line. A write error means the host hung up; the caller's read loop ends when
// the master closes.
func writePtyFrame(conn net.Conn, f vsock.PtyFrame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(b, '\n'))
}
