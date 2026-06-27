// Command crash-reap-smoke drives the real KVM-backed fork engine to prove the
// forkd crash-reconcile path on a real Firecracker process (fork-correctness
// section 7, issues #3 and #12). A first engine forks a sandbox (a real FC), then
// the engine is ABANDONED without Terminate, modelling a forkd crash that leaves
// the VM running and its journal on disk. A SECOND engine is constructed on the
// same data dir; its startup reconcile must either re-adopt the still-live VM and
// then reap it on Terminate, or, when the VM's pid is already dead, reap its
// leaked artifacts. Either way the assertion is: no orphaned Firecracker process
// and no leaked journal record or sandbox working dir survive.
//
// Two modes:
//
//	--mode adopt-reap : leave the forked FC ALIVE, build a second engine (which
//	    re-adopts it), Terminate it, and assert the pid is gone and the journal
//	    record + sandbox dir are removed.
//	--mode reap-dead  : SIGKILL the forked FC first, build a second engine, and
//	    assert its startup reconcile reaped the dead VM's journal record + dir
//	    without needing a Terminate call.
//
// This binary only does real work on a KVM host; it is built and invoked from the
// KVM workflow. It compiles on any platform so cross-build checks pass. A setup
// error exits 2; an assertion failure exits 1.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
)

func main() {
	image := flag.String("image", "", "rootfs.ext4 path (agent as /init) to build the template from")
	dataDir := flag.String("data-dir", "", "engine data directory")
	fcBin := flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
	kernel := flag.String("kernel", "", "path to the guest kernel (vmlinux)")
	agentBin := flag.String("agent-bin", "", "path to the guest agent binary")
	mode := flag.String("mode", "adopt-reap", "adopt-reap or reap-dead")
	flag.Parse()

	if *image == "" || *dataDir == "" || *kernel == "" || *agentBin == "" {
		fmt.Fprintln(os.Stderr, "crash-reap-smoke: --image, --data-dir, --kernel and --agent-bin are required")
		os.Exit(2)
	}

	if err := run(opts{image: *image, dataDir: *dataDir, fcBin: *fcBin, kernel: *kernel, agentBin: *agentBin, mode: *mode}); err != nil {
		fmt.Fprintf(os.Stderr, "crash-reap-smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("crash-reap-smoke: PASS: %s reconcile reaped the orphan with no leak\n", *mode)
}

type opts struct {
	image, dataDir, fcBin, kernel, agentBin, mode string
}

func run(o opts) error {
	if o.mode != "adopt-reap" && o.mode != "reap-dead" {
		return setupErr(fmt.Errorf("unknown --mode %q", o.mode))
	}

	newEngine := func() (*fork.Engine, error) {
		return fork.NewEngine(o.dataDir, o.fcBin, o.kernel, firecracker.JailerConfig{}, fork.EngineOpts{
			AllowUnverified: true,
			AgentBinPath:    o.agentBin,
		})
	}

	engine1, err := newEngine()
	if err != nil {
		return setupErr(fmt.Errorf("new engine 1: %w", err))
	}
	templateID := "crash-tmpl"
	if err := engine1.CreateTemplate(templateID, o.image, nil, nil, nil); err != nil {
		return setupErr(fmt.Errorf("create template: %w", err))
	}
	sandboxID := "crash-fork-1"
	if _, err := engine1.Fork(templateID, sandboxID, fork.ForkOpts{}); err != nil {
		return setupErr(fmt.Errorf("fork: %w", err))
	}

	recordPath := filepath.Join(o.dataDir, "sandboxes", sandboxID+".json")
	sandboxDir := filepath.Join(o.dataDir, "sandboxes", sandboxID)
	pid, err := journalPid(recordPath)
	if err != nil {
		return setupErr(fmt.Errorf("read journal record %s: %w", recordPath, err))
	}
	if !pidAlive(pid) {
		return setupErr(fmt.Errorf("forked FC pid %d is not alive right after fork", pid))
	}
	fmt.Printf("crash-reap-smoke: forked sandbox %s, FC pid %d, journal %s\n", sandboxID, pid, recordPath)

	// Model the crash: engine1 is ABANDONED with no Terminate, so the FC keeps
	// running and the journal record stays on disk, exactly the state a crashed
	// forkd leaves behind. (engine1 is intentionally not used again.)
	_ = engine1

	if o.mode == "reap-dead" {
		// The pre-crash VM died too: SIGKILL it so the second engine sees a dead
		// pid and must reap the leaked artifacts, not adopt+kill. A real restarted
		// forkd is a fresh process, so the dead FC is reparented to init and fully
		// reaped before the new forkd reconciles; in this single-process smoke the
		// abandoned engine1 is still alive, so the killed FC would linger as a
		// ZOMBIE (whose comm is still "firecracker", which the recycle guard's
		// comm fallback would wrongly adopt). Wait4 the zombie away so the pid is
		// genuinely gone before reconcile, matching the real crash.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var ws syscall.WaitStatus
			wpid, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
			if wpid == pid || !procExists(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("crash-reap-smoke: killed and reaped FC pid %d before reconcile (exists=%v)\n", pid, procExists(pid))
	}

	// The restarted forkd: a fresh engine on the same data dir. Its constructor
	// runs the crash reconcile (re-adopt live, or reap dead) before serving.
	engine2, err := newEngine()
	if err != nil {
		return setupErr(fmt.Errorf("new engine 2 (restarted forkd): %w", err))
	}

	if o.mode == "adopt-reap" {
		// The live VM must have been re-adopted; Terminate now reaps it (kills the
		// FC, removes its dir, drops the journal record). This is the GC-driven
		// kill of a re-adopted orphan.
		if err := engine2.Terminate(sandboxID); err != nil {
			return fmt.Errorf("terminate re-adopted sandbox: %w", err)
		}
	}

	// Assertions: no orphaned FC process, no leaked journal record, no leaked dir.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !pidReaped(pid) {
		time.Sleep(200 * time.Millisecond)
	}
	if !pidReaped(pid) {
		return fmt.Errorf("orphan FC pid %d still running after reconcile (mode %s): leaked process", pid, o.mode)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		return fmt.Errorf("journal record %s still present after reconcile (mode %s): leaked record (stat err=%v)", recordPath, o.mode, err)
	}
	if _, err := os.Stat(sandboxDir); !os.IsNotExist(err) {
		return fmt.Errorf("sandbox dir %s still present after reconcile (mode %s): leaked artifacts (stat err=%v)", sandboxDir, o.mode, err)
	}
	fmt.Printf("crash-reap-smoke: orphan reaped: pid %d gone, record + dir removed\n", pid)
	_ = engine2
	return nil
}

// journalPid reads the forkd journal record JSON and returns the recorded FC pid.
func journalPid(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path derived from the engine data dir, not user input
	if err != nil {
		return 0, err
	}
	var rec struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return 0, fmt.Errorf("decode journal record: %w", err)
	}
	if rec.Pid <= 0 {
		return 0, fmt.Errorf("journal record has no pid")
	}
	return rec.Pid, nil
}

// pidAlive reports whether pid names a live (non-zombie) process.
func pidAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	return !pidZombie(pid)
}

// pidReaped reports whether pid is gone or a zombie. In this single-process smoke
// the abandoned engine may not wait() on a killed FC, leaving a transient zombie;
// a zombie is reaped-enough for the no-orphan assertion (it holds no resources and
// is collected by init once the abandoned engine exits). A real restarted forkd
// is a fresh process, so the orphan reparents to init and disappears outright.
func pidReaped(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true // no such process
	}
	return pidZombie(pid)
}

// procExists reports whether /proc/<pid> still exists (the process or its zombie
// is still in the table).
func procExists(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// pidZombie reads /proc/<pid>/stat and reports whether the process state is Z.
func pidZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // gone counts as not-running
	}
	// stat: "pid (comm) STATE ...". comm may contain spaces/parens; split after ").
	s := string(data)
	if i := strings.LastIndex(s, ") "); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z'
	}
	return false
}

func setupErr(err error) error {
	fmt.Fprintf(os.Stderr, "crash-reap-smoke: SETUP: %v\n", err)
	os.Exit(2)
	return err
}
