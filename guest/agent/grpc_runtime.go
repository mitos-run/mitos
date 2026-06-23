//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mitos.run/mitos/internal/guestvitals"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// grpc_runtime.go implements the three genuinely-new guest gRPC RPCs that have
// no JSON-lines predecessor (Task 5.1c of issue #24): Watch (inotify), Processes
// (/proc), and Signal (kill). All three are linux-only and run as PID 1 inside
// the microVM, a NAMED security-sensitive path: each carries an explicit guard
// so a holder of the sandbox token cannot use them to read argv secrets, kill
// the in-guest control plane, or watch outside the workspace.

// processCPUSampleWindow is how long Processes samples each process's
// (utime+stime) jiffies and the aggregate /proc/stat total: two reads this far
// apart. It mirrors vitalsSampleWindow so the RPC stays well under the host's
// per-request deadline while still registering a busy process across at least
// one scheduler tick.
const processCPUSampleWindow = 100 * time.Millisecond

// Watch streams filesystem change events under the requested directory using
// inotify. SECURITY: the watched path is gated by pathAllowed (the same
// /workspace allowlist tardir.go enforces), so a caller can only watch inside
// the workspace and never the rootfs where the guest's secret/token state would
// be visible; an out-of-workspace path is rejected with PermissionDenied before
// any watch is added. The inotify fd is added with IN_DONT_FOLLOW so a symlink
// in the request path is not traversed out of the workspace.
//
// LIFECYCLE: a single inotify fd is created, the watch is added, and a reader
// goroutine drains it. When the client cancels (stream.Context() is Done) the
// fd is closed; the blocked unix.Read returns an error, the reader goroutine
// exits, and the watch is implicitly removed with the fd. There is no goroutine
// or fd leak on cancel. Event paths are filesystem names, not file content, and
// are not logged.
func (s *sandboxServer) Watch(req *sandboxv1.WatchRequest, stream sandboxv1.Sandbox_WatchServer) error {
	path := filepath.Clean(req.GetPath())
	if !pathAllowed(path) {
		return status.Errorf(codes.PermissionDenied, "watch: path %q is outside the workspace allowlist", req.GetPath())
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return status.Errorf(codes.NotFound, "watch: %v", err)
		}
		return status.Errorf(codes.Internal, "watch: %v", err)
	}
	if !info.IsDir() {
		return status.Errorf(codes.InvalidArgument, "watch: path %q is not a directory", path)
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return status.Errorf(codes.Internal, "watch: inotify init: %v", err)
	}
	// Closing the fd is the single teardown action: it removes the watch and
	// unblocks the reader goroutine's unix.Read. It is closed exactly once here
	// when the RPC returns (normal end or error), and also by the ctx watcher
	// below on client cancel; unix.Close is idempotent enough that a double close
	// only returns EBADF, which is ignored.
	defer unix.Close(fd) //nolint:errcheck // teardown; double close returns EBADF

	// IN_DONT_FOLLOW: do not follow a symlink at the watched path out of the
	// workspace. The mask covers create, modify, delete, and the move pair used to
	// derive RENAME and DELETE/CREATE.
	const mask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_DELETE |
		unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DONT_FOLLOW
	if _, err := unix.InotifyAddWatch(fd, path, mask); err != nil {
		return status.Errorf(codes.Internal, "watch: add watch: %v", err)
	}

	ctx := stream.Context()
	// ctx watcher: a client cancel (or normal stream end) closes the inotify fd so
	// the reader's blocked unix.Read returns and the loop below exits. The watcher
	// itself returns when ctx.Done fires, which it always does when the RPC ends,
	// so it never outlives this call.
	go func() {
		<-ctx.Done()
		_ = unix.Close(fd) //nolint:errcheck // unblock the reader; idempotent enough
	}()

	buf := make([]byte, 4096)
	// moveCookie correlates an IN_MOVED_FROM with its IN_MOVED_TO so a rename
	// within the watched dir is reported once as RENAME (old path -> new path).
	type pendingMove struct {
		name   string
		cookie uint32
	}
	var pending *pendingMove

	flushPendingDelete := func() error {
		if pending == nil {
			return nil
		}
		// A MOVED_FROM with no matching MOVED_TO is a move out of the dir: report it
		// as a DELETE of the old path.
		ev := &sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_DELETE, Path: filepath.Join(path, pending.name)}
		pending = nil
		return stream.Send(ev)
	}

	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			// EINTR: retry. Any other error after a ctx cancel is the expected fd
			// close; surface ctx.Err() so the client sees Canceled, not Internal.
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if ctx.Err() != nil {
				if ferr := flushPendingDelete(); ferr != nil {
					return ferr
				}
				return ctx.Err()
			}
			return status.Errorf(codes.Internal, "watch: read: %v", err)
		}
		offset := 0
		for offset+unix.SizeofInotifyEvent <= n {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameLen := int(raw.Len)
			var name string
			if nameLen > 0 {
				nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+nameLen]
				// The name is NUL-padded to Len; trim at the first NUL.
				if i := bytes.IndexByte(nameBytes, 0); i >= 0 {
					nameBytes = nameBytes[:i]
				}
				name = string(nameBytes)
			}
			offset += unix.SizeofInotifyEvent + nameLen

			full := filepath.Join(path, name)
			mask := raw.Mask
			switch {
			case mask&unix.IN_MOVED_FROM != 0:
				// A new MOVED_FROM supersedes any unmatched prior one (report the old as
				// a DELETE first).
				if err := flushPendingDelete(); err != nil {
					return err
				}
				pending = &pendingMove{name: name, cookie: raw.Cookie}
			case mask&unix.IN_MOVED_TO != 0:
				if pending != nil && pending.cookie == raw.Cookie {
					// A rename within the watched dir: old -> new.
					ev := &sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_RENAME, Path: filepath.Join(path, pending.name), NewPath: full}
					pending = nil
					if err := stream.Send(ev); err != nil {
						return err
					}
				} else {
					// A move into the dir with no matching MOVED_FROM: a CREATE.
					if err := stream.Send(&sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_CREATE, Path: full}); err != nil {
						return err
					}
				}
			case mask&unix.IN_CREATE != 0:
				if err := flushPendingDelete(); err != nil {
					return err
				}
				if err := stream.Send(&sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_CREATE, Path: full}); err != nil {
					return err
				}
			case mask&unix.IN_DELETE != 0:
				if err := flushPendingDelete(); err != nil {
					return err
				}
				if err := stream.Send(&sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_DELETE, Path: full}); err != nil {
					return err
				}
			case mask&unix.IN_MODIFY != 0:
				if err := flushPendingDelete(); err != nil {
					return err
				}
				if err := stream.Send(&sandboxv1.FsEvent{Kind: sandboxv1.FsEvent_MODIFY, Path: full}); err != nil {
					return err
				}
			}
		}
		// An unmatched MOVED_FROM at the end of a read batch is a move out of the
		// dir; report it as a DELETE so the event is not silently dropped.
		if err := flushPendingDelete(); err != nil {
			return err
		}
	}
}

// Processes returns the in-guest process table read from /proc. SECURITY: the
// command field is the process NAME (comm, /proc/<pid>/stat field 2), NEVER the
// full /proc/<pid>/cmdline. cmdline can contain SECRETS passed as argv (API
// keys, tokens, connection strings); exposing it would leak those secrets to any
// holder of the sandbox token. comm is the bare program name and is already
// visible to anyone who can exec in the guest, so it is safe to report. This
// reuses the same guestvitals.ParsePidStat the vitals process list uses.
//
// cpu_percent is computed over a short window: two passes of (utime+stime) per
// pid and the aggregate /proc/stat total, so the value is the process's share of
// wall CPU across the window rather than a misleading lifetime average.
func (s *sandboxServer) Processes(_ context.Context, _ *sandboxv1.ProcessesRequest) (*sandboxv1.ProcessList, error) {
	first, totalFirst, err := snapshotProcesses()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "processes: %v", err)
	}
	time.Sleep(processCPUSampleWindow)
	second, totalSecond, err := snapshotProcesses()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "processes: %v", err)
	}

	totalDelta := float64(0)
	if totalSecond > totalFirst {
		totalDelta = float64(totalSecond - totalFirst)
	}

	pageKB := uint64(guestPageSize) / 1024
	if pageKB == 0 {
		pageKB = 4 // sane fallback; standard 4KB page
	}

	out := &sandboxv1.ProcessList{}
	for pid, p := range second {
		var cpuPercent float64
		if prev, ok := first[pid]; ok && totalDelta > 0 {
			procDelta := float64((p.UTime + p.STime) - (prev.UTime + prev.STime))
			if procDelta > 0 {
				cpuPercent = procDelta / totalDelta * 100
			}
		}
		out.Processes = append(out.Processes, &sandboxv1.ProcessInfo{
			Pid:        int32(p.PID),
			Ppid:       int32(p.PPID),
			Command:    p.Comm, // comm only; NEVER cmdline (argv may carry secrets)
			State:      p.State,
			CpuPercent: cpuPercent,
			RssBytes:   int64(p.RSSPages*pageKB) * 1024,
		})
	}
	return out, nil
}

// snapshotProcesses reads every numeric pid directory under procRoot into a map
// keyed by pid, alongside the aggregate /proc/stat total jiffies for the same
// instant. A pid that vanishes mid-walk is skipped, since the table is racy.
func snapshotProcesses() (map[int]guestvitals.PidStat, uint64, error) {
	stat, err := readProcStat()
	if err != nil {
		return nil, 0, fmt.Errorf("read proc stat: %w", err)
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, 0, fmt.Errorf("read procfs: %w", err)
	}
	out := make(map[int]guestvitals.PidStat, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue // pid exited mid-walk
		}
		p, err := guestvitals.ParsePidStat(data)
		if err != nil {
			continue
		}
		out[p.PID] = p
	}
	return out, stat.Total(), nil
}

// Signal sends a POSIX signal to a process inside the guest. SECURITY: PID 1 is
// the guest agent itself (this control plane); signalling it with a kill signal
// would let a caller terminate the in-VM control plane, so any pid <= 1 is
// rejected with InvalidArgument. The signal number is range-checked against the
// valid POSIX signal range before the syscall so an out-of-range value cannot be
// passed to the kernel. No argv, env, or secret value is involved or logged.
func (s *sandboxServer) Signal(_ context.Context, req *sandboxv1.SignalRequest) (*sandboxv1.SignalResponse, error) {
	pid := req.GetPid()
	if pid <= 1 {
		// pid 1 is the guest agent (the in-VM control plane); pid 0 and negatives
		// address process groups and are not a single-process signal target here.
		return nil, status.Errorf(codes.InvalidArgument, "signal: refusing to signal pid %d: pid 1 is the guest control plane and pids <= 1 are not addressable", pid)
	}
	sig := req.GetSignal()
	if sig < 1 || sig > maxSignal {
		return nil, status.Errorf(codes.InvalidArgument, "signal: signal number %d out of range 1..%d", sig, maxSignal)
	}
	if err := unix.Kill(int(pid), unix.Signal(sig)); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, status.Errorf(codes.NotFound, "signal: no such process %d", pid)
		}
		if errors.Is(err, unix.EPERM) {
			return nil, status.Errorf(codes.PermissionDenied, "signal: not permitted to signal pid %d", pid)
		}
		return nil, status.Errorf(codes.Internal, "signal: kill(%d, %d): %v", pid, sig, err)
	}
	return &sandboxv1.SignalResponse{}, nil
}

// maxSignal is the upper bound on a valid POSIX signal number. The Linux kernel
// signal space runs 1..64 (the standard signals plus the SIGRTMIN..SIGRTMAX
// real-time range, _NSIG-1); bounding here keeps a caller from passing a
// nonsense value into the kill syscall while still allowing the full range.
const maxSignal int32 = 64
