// Command bench measures the sandbox fork and exec data path directly against
// the real KVM-backed engine. It is the reproducible source behind every
// latency number the project publishes (CLAUDE.md operating principle 1).
//
// Driver path: bench imports internal/fork and internal/vsock and drives the
// engine in-process. This is the most direct measurement of the data path: it
// forks from a template snapshot already present under --data-dir (the CI
// builds it), connects to the fork's Firecracker vsock UDS, and execs a
// trivial command. There is no forkd and no HTTP API in the path, so
// the timing reflects fork + vsock + guest agent and nothing else.
//
// The exec round-trip uses the gRPC runtime protocol: a Control.Ping RPC on
// AgentGRPCPort 53, served by the Rust guest agent. The legacy JSON-lines guest
// protocol and the Go agent were removed (#310), so gRPC on port 53 is the only
// transport. The --exec-transport flag is retained for backward compatibility
// of existing invocations: both its "grpc" (default) and "json" values now drive
// the same gRPC path.
//
// The engine validates /dev/kvm at construction, so the timing path runs only
// on a Linux KVM host; on any other platform the tool builds and parses flags
// but exits non-zero at engine construction with a clear message.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"mitos.run/mitos/internal/benchstat"
	"mitos.run/mitos/internal/cpupin"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/metering"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
)

const (
	modeForkExec    = "fork-exec"
	modeExecRT      = "exec-rt"
	modeMetering    = "metering"
	modeForkFanOut  = "fork-fanout"
	modePrefetch    = "prefetch"
	modePinning     = "pinning"
	defaultFanOutNs = "1,4,16,64"

	// execTransportGRPC drives the gRPC Control.Ping RPC over AgentGRPCPort 53,
	// served by the Rust guest agent. This is the only runtime transport and the
	// default.
	execTransportGRPC = "grpc"
	// execTransportJSON names the removed JSON-lines transport. The legacy JSON
	// guest protocol and the Go agent are gone (#310); this value is accepted for
	// backward compatibility of existing invocations and now drives the same gRPC
	// path as "grpc".
	execTransportJSON = "json"
)

// config holds the parsed, validated flags. Parsing is split out so it can be
// unit-tested without touching the KVM-only timing path.
type config struct {
	mode        string
	iterations  int
	warmup      int
	template    string
	dataDir     string
	firecracker string
	kernel      string
	jsonPath    string
	summary     bool
	// execTransport is retained for backward compatibility of existing bench
	// invocations. The exec round-trip always uses the gRPC Control.Ping RPC on
	// AgentGRPCPort 53 (the Rust guest agent); the legacy JSON-lines guest
	// protocol and the Go agent were removed (#310). Both "grpc" (default) and
	// "json" drive the same gRPC path.
	execTransport string
	// forks is the number of sandboxes the metering mode forks from one
	// template before reading the CoW-aware metering report. It is unused by
	// the latency modes.
	forks    int
	settleMs int
	// fanOutN is the list of fan-out widths (N) the fork-fanout mode measures:
	// fork ONE warmed base into N children and report the per-child
	// time-to-ready distribution plus the wall clock to all N ready. Unused by
	// the other modes.
	fanOutN []int
}

// parseConfig parses args (excluding the program name) into a validated config.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)

	var cfg config
	var fanOutNs string
	fs.StringVar(&cfg.mode, "mode", modeForkExec, "benchmark mode: fork-exec|exec-rt|metering|fork-fanout|prefetch|pinning")
	fs.IntVar(&cfg.iterations, "iterations", 50, "measured iterations")
	fs.IntVar(&cfg.warmup, "warmup", 5, "discarded warmup iterations; in exec-rt mode one mandatory connection-establishment exec always runs in addition to these, even at --warmup=0")
	fs.StringVar(&cfg.template, "template", "", "template (snapshot) id to fork from")
	fs.StringVar(&cfg.dataDir, "data-dir", "/var/lib/mitos", "data directory holding template snapshots")
	fs.StringVar(&cfg.firecracker, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	fs.StringVar(&cfg.kernel, "kernel", "/var/lib/mitos/vmlinux", "guest kernel path")
	fs.StringVar(&cfg.jsonPath, "json", "", "optional path to write results JSON")
	fs.BoolVar(&cfg.summary, "summary", false, "print the summary table to stdout")
	fs.IntVar(&cfg.forks, "forks", 4, "metering mode: number of sandboxes to fork from one template before reading the report")
	fs.IntVar(&cfg.settleMs, "settle-ms", 500, "metering mode: milliseconds to let the forks settle before reading the report")
	fs.StringVar(&fanOutNs, "fanout-n", defaultFanOutNs, "fork-fanout mode: comma-separated fan-out widths (N) to measure, e.g. 1,4,16,64")
	fs.StringVar(&cfg.execTransport, "exec-transport", execTransportGRPC, "retained for backward compatibility: the exec round-trip always uses gRPC Control.Ping over AgentGRPCPort 53 (Rust guest agent); the legacy JSON transport and Go agent were removed (#310), so grpc (default) and json both drive the gRPC path")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.mode != modeForkExec && cfg.mode != modeExecRT && cfg.mode != modeMetering && cfg.mode != modeForkFanOut && cfg.mode != modePrefetch && cfg.mode != modePinning {
		return config{}, fmt.Errorf("invalid --mode %q: want %s, %s, %s, %s, %s, or %s", cfg.mode, modeForkExec, modeExecRT, modeMetering, modeForkFanOut, modePrefetch, modePinning)
	}
	if cfg.execTransport != execTransportJSON && cfg.execTransport != execTransportGRPC {
		return config{}, fmt.Errorf("invalid --exec-transport %q: want %s or %s", cfg.execTransport, execTransportJSON, execTransportGRPC)
	}
	if cfg.template == "" {
		return config{}, fmt.Errorf("--template is required")
	}
	if cfg.iterations < 1 {
		return config{}, fmt.Errorf("--iterations must be at least 1, got %d", cfg.iterations)
	}
	if cfg.warmup < 0 {
		return config{}, fmt.Errorf("--warmup must not be negative, got %d", cfg.warmup)
	}
	if cfg.mode == modeMetering {
		if cfg.forks < 1 {
			return config{}, fmt.Errorf("--forks must be at least 1 in metering mode, got %d", cfg.forks)
		}
		if cfg.settleMs < 0 {
			return config{}, fmt.Errorf("--settle-ms must not be negative, got %d", cfg.settleMs)
		}
	}
	if cfg.mode == modeForkFanOut {
		ns, err := parseFanOutNs(fanOutNs)
		if err != nil {
			return config{}, err
		}
		cfg.fanOutN = ns
	}
	return cfg, nil
}

// parseFanOutNs parses a comma-separated list of fan-out widths (for example
// "1,4,16,64") into a slice of positive ints. Each entry must be a positive
// integer; the list must be non-empty.
func parseFanOutNs(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	ns := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("--fanout-n entry is empty: %q", s)
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("--fanout-n entry %q is not an integer: %w", p, err)
		}
		if v < 1 {
			return nil, fmt.Errorf("--fanout-n entries must be at least 1, got %d", v)
		}
		ns = append(ns, v)
	}
	if len(ns) == 0 {
		return nil, fmt.Errorf("--fanout-n must list at least one width")
	}
	return ns, nil
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	// Mirror cmd/forkd construction with a zero jailer config (jailer
	// disabled) and networking/CAS opts left at their defaults: the bench
	// measures the bare fork + exec path, so no per-fork network is set up.
	engine, err := fork.NewEngine(cfg.dataDir, cfg.firecracker, cfg.kernel, firecracker.JailerConfig{}, fork.EngineOpts{})
	if err != nil {
		return fmt.Errorf("init engine (needs Linux + /dev/kvm + template under --data-dir): %w", err)
	}

	// Metering mode forks N real sandboxes from one template and prints the
	// CoW-aware metering report; it does not produce a latency distribution.
	if cfg.mode == modeMetering {
		return runMetering(engine, cfg)
	}

	// Fork-fanout mode forks ONE warmed base into N children at several N and
	// reports the per-child time-to-ready distribution plus the wall clock to
	// all N ready; it produces FanOutResults, not a single latency Result.
	if cfg.mode == modeForkFanOut {
		return runForkFanOut(engine, cfg)
	}

	// Prefetch mode measures the snapshot-resume page-fault prefetch win (issue
	// #167): fault count per resume and claim->first-exec, prefetch on vs off.
	if cfg.mode == modePrefetch {
		return runPrefetch(engine, cfg)
	}

	// Pinning mode measures the dynamic CPU pinning + launch RT priority win
	// (issue #168): activate success rate and activate latency under a claim
	// storm, pinning on vs off.
	if cfg.mode == modePinning {
		return runPinning(engine, cfg)
	}

	var result benchstat.Result
	switch cfg.mode {
	case modeForkExec:
		result, err = benchForkExec(engine, cfg)
	case modeExecRT:
		result, err = benchExecRT(engine, cfg)
	default:
		return fmt.Errorf("invalid mode %q", cfg.mode)
	}
	if err != nil {
		return err
	}

	results := []benchstat.Result{result}

	if cfg.summary {
		fmt.Printf("%s (%s)\n%s", result.Name, result.Unit, result.Summary.Table())
	}
	if cfg.jsonPath != "" {
		f, err := os.Create(cfg.jsonPath)
		if err != nil {
			return fmt.Errorf("create json output: %w", err)
		}
		defer f.Close()
		if err := benchstat.WriteJSON(f, results); err != nil {
			return err
		}
	}
	return nil
}

// runMetering forks cfg.forks real sandboxes from one template, lets them
// settle, then reads the engine's CoW-aware metering report and prints it as
// JSON (machine-readable for the CI jq assertions) plus a human summary. The
// forks are NOT torn down before the report is read: the whole point is to
// observe N concurrent forks of one template sharing the same restored page
// set, so the shared template region is counted once and the per-fork marginal
// cost is the unique set. Every fork is torn down after the report is captured.
//
// This proves metering correctness AND yields an honest density datapoint: the
// shared template footprint is paid once, and each additional fork adds only
// its unique (private-dirty) pages.
func runMetering(engine *fork.Engine, cfg config) error {
	forked := make([]string, 0, cfg.forks)
	// Tear every fork down on the way out, success or failure, so a metering
	// run never leaks VMs on the runner.
	defer func() {
		for _, id := range forked {
			_ = engine.Terminate(id)
		}
	}()

	for i := 0; i < cfg.forks; i++ {
		id := fmt.Sprintf("meter-%d", i)
		if _, err := engine.Fork(cfg.template, id, fork.ForkOpts{}); err != nil {
			return fmt.Errorf("fork %d of %d: %w", i+1, cfg.forks, err)
		}
		forked = append(forked, id)
	}

	// Let the forks settle so their resident set reflects a steady restored
	// state rather than the instant after restore.
	if cfg.settleMs > 0 {
		time.Sleep(time.Duration(cfg.settleMs) * time.Millisecond)
	}

	report := engine.Metering()

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metering report: %w", err)
	}
	// The JSON is the contract the CI jq assertions parse; print it on its own.
	fmt.Println(string(out))

	if cfg.summary {
		printMeteringSummary(report, cfg.forks)
	}
	if cfg.jsonPath != "" {
		if err := os.WriteFile(cfg.jsonPath, out, 0o644); err != nil {
			return fmt.Errorf("write metering json: %w", err)
		}
	}
	return nil
}

// printMeteringSummary prints a short human-readable summary of the CoW-aware
// metering report to stdout. All numbers are derived from the real engine
// report (smaps-derived memory, stat-derived disk); nothing is invented.
func printMeteringSummary(report metering.Report, forks int) {
	mib := func(b int64) float64 { return float64(b) / (1024 * 1024) }
	fmt.Printf("\n=== CoW-aware metering: %d fork(s) of one template ===\n", forks)
	fmt.Printf("  sandboxes:        %d\n", len(report.Sandboxes))
	fmt.Printf("  templates:        %d\n", len(report.Templates))
	fmt.Printf("  TotalUnique:      %.2f MiB (sum of every fork's private-dirty set)\n", mib(report.TotalUnique))
	fmt.Printf("  UsedCoWAware:     %.2f MiB (unique + each template's shared set counted once)\n", mib(report.UsedCoWAware))
	fmt.Printf("  UsedNaive:        %.2f MiB (unique + every fork's shared set, double-counted)\n", mib(report.UsedNaive))
	fmt.Printf("  CoWSavings:       %.2f MiB (naive - CoW-aware)\n", mib(report.CoWSavings))
	for _, t := range report.Templates {
		fmt.Printf("  template %q: forks=%d sharedOnce=%.2f MiB diskSharedOnce=%.2f MiB\n",
			t.Template, t.ForkCount, mib(t.SharedOnce), mib(t.DiskSharedOnce))
	}
	for _, s := range report.Sandboxes {
		fmt.Printf("  fork %q: unique=%.2f MiB shared=%.2f MiB\n",
			s.ID, mib(s.MemoryUnique), mib(s.MemoryShared))
	}
}

// runPrefetch measures the snapshot-resume page-fault prefetch win (issue #167):
// for the prefetch-OFF arm (lazy-fault baseline) and the prefetch-ON arm
// (the captured hot-page set preloaded by the userfaultfd handler before
// resume), it records the page-fault count per resume and the claim->first-exec
// latency, then reports the per-arm distributions and the fault-count reduction
// via the pure, unit-tested benchstat.AggregatePrefetch.
//
// HONEST GATING: the per-resume fault count comes from the userfaultfd handler
// (internal/fork prefetch_linux.go), which needs a live KVM host with
// hugepage-backed guest memory and is not yet wired (the syscall-level
// register/copy is the bare-metal follow-up). So this mode does NOT fabricate a
// number: it returns a clear not-yet-measurable error rather than inventing
// fault counts or latencies. On a non-KVM host the engine already failed to
// construct in run() before this is ever reached. Once the handler lands, the
// collection loop here forks with prefetch off then on, reads the handler's
// fault count per resume and the claim->first-exec span, and hands both arms to
// AggregatePrefetch; the aggregation and JSON shape are testable today.
func runPrefetch(engine *fork.Engine, cfg config) error {
	// Capture the template's hot-page working set once (off the measured path),
	// stamping it onto the snapshot manifest, then run two arms over the real
	// UFFD-backed restore: OFF disables preload (lazy faults), ON preloads the
	// captured set before resume. Each arm records the per-resume page-fault count
	// the userfaultfd handler serviced and the claim->first-exec latency; the pure,
	// unit-tested benchstat.AggregatePrefetch turns the two arms into the headline
	// fault reduction. No number is fabricated: the fault counts come from the real
	// handler, and off any non-KVM host the engine failed to construct in run().
	if _, err := engine.CaptureTemplateHotPages(cfg.template, 0); err != nil {
		return fmt.Errorf("prefetch mode (#167): capture hot-page set for %q: %w", cfg.template, err)
	}

	off, err := prefetchArm(engine, cfg, true)
	if err != nil {
		return fmt.Errorf("prefetch OFF arm: %w", err)
	}
	on, err := prefetchArm(engine, cfg, false)
	if err != nil {
		return fmt.Errorf("prefetch ON arm: %w", err)
	}

	cmp := benchstat.AggregatePrefetch(off, on)
	if cfg.summary {
		fmt.Print(cmp.Table())
	}
	if cfg.jsonPath != "" {
		f, err := os.Create(cfg.jsonPath)
		if err != nil {
			return fmt.Errorf("create json output: %w", err)
		}
		defer f.Close()
		if err := benchstat.WritePrefetchJSON(f, cmp); err != nil {
			return err
		}
	}
	return nil
}

// prefetchArm forks the template cfg.iterations times (after cfg.warmup discarded
// warmups), execs a trivial command to mark first-exec, and records the per-resume
// page-fault count and claim->first-exec latency. disablePrefetch selects the OFF
// arm (lazy faults) vs the ON arm (hot-page set preloaded before resume).
func prefetchArm(engine *fork.Engine, cfg config, disablePrefetch bool) (benchstat.PrefetchArm, error) {
	arm := benchstat.PrefetchArm{}
	tag := "on"
	if disablePrefetch {
		tag = "off"
	}
	for i := 0; i < cfg.warmup; i++ {
		id := fmt.Sprintf("pf-%s-warm-%d", tag, i)
		if _, _, err := onePrefetchForkExec(engine, cfg.template, id, disablePrefetch); err != nil {
			return arm, fmt.Errorf("warmup %d: %w", i, err)
		}
	}
	for i := 0; i < cfg.iterations; i++ {
		id := fmt.Sprintf("pf-%s-%d", tag, i)
		faults, elapsed, err := onePrefetchForkExec(engine, cfg.template, id, disablePrefetch)
		if err != nil {
			return arm, fmt.Errorf("iteration %d: %w", i, err)
		}
		arm.FaultCounts = append(arm.FaultCounts, faults)
		arm.ClaimToExec = append(arm.ClaimToExec, elapsed)
	}
	return arm, nil
}

// runPinning measures the dynamic CPU pinning + launch RT priority win (issue
// #168): activate success rate and activate latency under a claim storm,
// pinning ON vs OFF. The headline number is the activate-success-rate lift
// under storm; the aggregation (success rate + latency distribution per arm,
// the on-vs-off lift) is the pure, unit-tested benchstat.AggregatePinning.
//
// HONEST GATING: the success-rate and latency numbers require Linux + KVM +
// bare metal + a REAL claim storm (sched_setaffinity and the RT class are
// Linux-only, and the density/activate-success effect only appears under real
// contention). darwin cannot apply a pin at all (the cpupin applier is a no-op
// stub there), so this mode refuses to emit a number off a supporting host. On
// a non-KVM host the engine already failed to construct in run() before this is
// reached; here we additionally refuse when the applier reports it cannot change
// scheduler state (darwin) or when the claim-storm harness is not yet wired
// (the bare-metal follow-up, #16). No number is fabricated.
func runPinning(engine *fork.Engine, cfg config) error {
	if !cpupin.NewApplier().Supported() {
		return fmt.Errorf(
			"pinning mode (issue #168) cannot measure on this platform: CPU pinning and RT scheduling priority are Linux-only and the applier is a no-op here; the on-vs-off aggregation (benchstat.AggregatePinning) is in place and unit-tested, the real claim-storm measurement needs Linux + KVM (template=%q, iterations=%d)",
			cfg.template, cfg.iterations,
		)
	}
	// Real claim storm: each round forks cfg.forks sandboxes CONCURRENTLY (the
	// contention that makes pinning matter), execs each to first-exec, and records
	// the per-activation outcome (success + latency). The OFF arm leaves forks
	// unpinned; the ON arm pins each fork's vCPU threads after guest-ready
	// (sibling-paired). benchstat.AggregatePinning turns the two arms into the
	// success-rate lift and the activate-latency distributions. Outcomes come from
	// the real engine under real contention; nothing is fabricated.
	off, err := pinningStormArm(engine, cfg, nil)
	if err != nil {
		return fmt.Errorf("pinning OFF arm: %w", err)
	}
	on, err := pinningStormArm(engine, cfg, &cpupin.Config{Enabled: true, SiblingPairing: true})
	if err != nil {
		return fmt.Errorf("pinning ON arm: %w", err)
	}
	cmp := benchstat.AggregatePinning(off, on)
	if cfg.summary {
		fmt.Print(cmp.Table())
	}
	if cfg.jsonPath != "" {
		f, err := os.Create(cfg.jsonPath)
		if err != nil {
			return fmt.Errorf("create json output: %w", err)
		}
		defer f.Close()
		if err := benchstat.WritePinningJSON(f, cmp); err != nil {
			return err
		}
	}
	return nil
}

// pinningStormArm runs cfg.iterations storm rounds; each round forks cfg.forks
// sandboxes CONCURRENTLY (a claim storm), execs each to first-exec, and records
// one ActivateOutcome per fork. pin nil leaves the round unpinned (OFF arm); a
// non-nil pin enables post-ready CPU pinning (ON arm). All forks in a round are
// torn down before the next round so contention is per-round, not cumulative.
func pinningStormArm(engine *fork.Engine, cfg config, pin *cpupin.Config) (benchstat.PinningArm, error) {
	arm := benchstat.PinningArm{}
	for round := 0; round < cfg.iterations; round++ {
		outcomes := make([]benchstat.ActivateOutcome, cfg.forks)
		var wg sync.WaitGroup
		for i := 0; i < cfg.forks; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				id := fmt.Sprintf("storm-r%d-%d", round, idx)
				outcomes[idx] = oneStormActivate(engine, cfg.template, id, pin)
			}(i)
		}
		wg.Wait()
		arm.Outcomes = append(arm.Outcomes, outcomes...)
	}
	return arm, nil
}

// oneStormActivate forks one sandbox (optionally pinned), calls Control.Ping over
// gRPC to mark first-response, and returns the activation outcome (success +
// fork->ping latency). Any fork/connect/ping failure is recorded as a failed
// activation, so the storm's success RATE is meaningful under contention. The
// sandbox is always torn down.
func oneStormActivate(engine *fork.Engine, template, sandboxID string, pin *cpupin.Config) benchstat.ActivateOutcome {
	t0 := time.Now()
	opts := fork.ForkOpts{VCPUs: 1}
	if pin != nil {
		opts.CPUPinning = pin
	}
	res, err := engine.Fork(template, sandboxID, opts)
	if err != nil {
		return benchstat.ActivateOutcome{OK: false}
	}
	defer func() { _ = engine.Terminate(sandboxID) }()
	cc, err := connectGRPCWithRetry(res.VsockPath)
	if err != nil {
		return benchstat.ActivateOutcome{OK: false}
	}
	defer func() { _ = cc.Close() }()
	return benchstat.ActivateOutcome{OK: true, Latency: time.Since(t0)}
}

// runForkFanOut measures the 1-to-N live-fork fan-out shape (issue #207): fork
// ONE warmed base (the template snapshot, which already has the repo loaded and
// deps installed when the maintainer builds it that way) into N children, and
// for each N in cfg.fanOutN record (a) each child's time-to-ready (fork ->
// first successful exec) and (b) the wall clock from the fan-out start to the
// instant the LAST child is ready. The defensible Mitos claim under test is
// sub-second 1-to-N COW fan-out, so the headline number is wall-clock-to-N-ready
// at the larger N.
//
// Children are forked sequentially from the one base on a single shared wall
// clock: each child's ReadyOffset is measured from the fan-out start, so the
// max ReadyOffset is the honest wall-clock-to-N-ready. This drives the REAL
// engine in-process exactly like the other modes; on a host without /dev/kvm
// the engine fails at construction in run() before this is ever reached, so
// fork-fanout never emits a fabricated number off-KVM.
//
// The per-N aggregation (per-child distribution + wall-clock-to-N-ready) is the
// pure, unit-tested benchstat.AggregateFanOut; this function only collects the
// real samples and hands them to it.
func runForkFanOut(engine *fork.Engine, cfg config) error {
	results := make([]benchstat.FanOutResult, 0, len(cfg.fanOutN))
	for _, n := range cfg.fanOutN {
		fo, err := oneFanOut(engine, cfg.template, n)
		if err != nil {
			return fmt.Errorf("fan-out N=%d: %w", n, err)
		}
		results = append(results, benchstat.FanOutResult{N: n, Name: "fork_fanout", FanOut: fo})

		if cfg.summary {
			fmt.Printf("=== fork-fanout N=%d ===\n%s\n", n, fo.Table())
		}
	}

	if cfg.jsonPath != "" {
		f, err := os.Create(cfg.jsonPath)
		if err != nil {
			return fmt.Errorf("create json output: %w", err)
		}
		defer f.Close()
		if err := benchstat.WriteFanOutJSON(f, results); err != nil {
			return err
		}
	}
	return nil
}

// oneFanOut forks one base into n children, measuring each child's
// fork->first-exec time-to-ready on a shared wall clock, then tears every child
// down. The base itself is the template snapshot, so every child is a live COW
// fork of the same warmed state. Children are torn down only after all of them
// have reached ready and the wall clock has been read, so teardown never
// inflates the measured wall-clock-to-N-ready.
func oneFanOut(engine *fork.Engine, template string, n int) (benchstat.FanOut, error) {
	forked := make([]string, 0, n)
	// Tear every child down on the way out, success or failure, so a fan-out
	// run never leaks VMs on the runner.
	defer func() {
		for _, id := range forked {
			_ = engine.Terminate(id)
		}
	}()

	children := make([]benchstat.ChildReady, 0, n)
	fanStart := time.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fanout-%d-%d", n, i)
		childStart := time.Now()
		ttr, err := forkToReady(engine, template, id)
		if err != nil {
			return benchstat.FanOut{}, fmt.Errorf("child %d of %d: %w", i+1, n, err)
		}
		// The child is ready; record it on the books for teardown. Its
		// ReadyOffset is the wall-clock instant from the fan-out start; its
		// TimeToReady is its own fork->ready span.
		forked = append(forked, id)
		children = append(children, benchstat.ChildReady{
			TimeToReady: ttr,
			ReadyOffset: childStart.Add(ttr).Sub(fanStart),
		})
	}

	return benchstat.AggregateFanOut(children), nil
}

// forkToReady forks one child off the base and returns the time from fork start
// to the first successful exec result (the child's time-to-ready). It does NOT
// tear the child down: the caller keeps every child alive for the duration of
// the fan-out so that N live COW forks coexist, which is the whole point of the
// 1-to-N shape. The clock starts immediately before Fork and stops the instant
// the first exec result is in.
func forkToReady(engine *fork.Engine, template, sandboxID string) (time.Duration, error) {
	t0 := time.Now()
	res, err := engine.Fork(template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return 0, fmt.Errorf("fork: %w", err)
	}
	// connectGRPCWithRetry dials and verifies with a Control.Ping, so the
	// returned conn has already received a ping response. The clock stops here.
	cc, err := connectGRPCWithRetry(res.VsockPath)
	if err != nil {
		return 0, fmt.Errorf("connect: %w", err)
	}
	elapsed := time.Since(t0) // clock stops when the first ping is in
	_ = cc.Close()
	return elapsed, nil
}

// benchForkExec measures the time from fork start to the first successful
// Control.Ping response over gRPC (AgentGRPCPort 53), terminating the sandbox
// each iteration. The json flag value is accepted for backward compatibility but
// also uses the gRPC path: the legacy JSON agent was removed (#310).
func benchForkExec(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	// Warmup iterations are discarded; they pay the page-cache and
	// snapshot-load costs that should not skew the measured samples.
	for i := 0; i < cfg.warmup; i++ {
		id := fmt.Sprintf("bench-warm-%d", i)
		if _, err := oneForkExecGRPC(engine, cfg.template, id); err != nil {
			return benchstat.Result{}, fmt.Errorf("warmup iteration %d: %w", i, err)
		}
	}

	samples := make([]time.Duration, 0, cfg.iterations)
	for i := 0; i < cfg.iterations; i++ {
		id := fmt.Sprintf("bench-fe-%d", i)
		elapsed, err := oneForkExecGRPC(engine, cfg.template, id)
		if err != nil {
			return benchstat.Result{}, fmt.Errorf("iteration %d: %w", i, err)
		}
		samples = append(samples, elapsed)
	}

	return benchstat.Result{Name: "fork_to_first_grpc_ping", Unit: "ms", Summary: benchstat.Summarize(samples)}, nil
}

// onePrefetchForkExec forks one sandbox (UFFD-backed; disablePrefetch selects the
// lazy OFF arm vs the preloaded ON arm), pings over gRPC to mark first-response,
// and returns the userfaultfd lazy-fault count for the resume and the
// claim->first-ping latency. The sandbox is always torn down before returning so
// no iteration leaks a VM. The fault count is read AFTER the ping (faults accrue
// as the guest runs) and BEFORE teardown.
func onePrefetchForkExec(engine *fork.Engine, template, sandboxID string, disablePrefetch bool) (int, time.Duration, error) {
	t0 := time.Now()
	res, err := engine.Fork(template, sandboxID, fork.ForkOpts{DisablePrefetch: disablePrefetch})
	if err != nil {
		return 0, 0, fmt.Errorf("fork: %w", err)
	}
	cleanup := func() { _ = engine.Terminate(sandboxID) }

	// connectGRPCWithRetry retries until the agent is reachable and verifies
	// liveness via a Control.Ping, so the clock already covers the first ping.
	cc, err := connectGRPCWithRetry(res.VsockPath)
	if err != nil {
		cleanup()
		return 0, 0, fmt.Errorf("connect: %w", err)
	}
	elapsed := time.Since(t0)
	faults := int(engine.FaultsServed(sandboxID))
	_ = cc.Close()
	cleanup()
	return faults, elapsed, nil
}

// benchExecRT measures M trivial Control.Ping round-trips over gRPC against a
// forked sandbox. The --exec-transport flag is accepted for backward
// compatibility; both json and grpc values use the gRPC path because the
// legacy JSON agent was removed (#310).
func benchExecRT(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	return benchExecRTGRPC(engine, cfg)
}

// benchExecRTGRPC forks one sandbox, warms up the gRPC connection, then
// measures M Control.Ping round-trips over gRPC. A single *grpc.ClientConn is
// reused across all iterations: grpc-go reuses the underlying transport, so
// each Ping call is a new RPC on the same HTTP/2 connection. This matches how
// the JSON exec-rt reuses one vsock connection across all iterations.
func benchExecRTGRPC(engine *fork.Engine, cfg config) (benchstat.Result, error) {
	const sandboxID = "bench-execrt-grpc"
	res, err := engine.Fork(cfg.template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return benchstat.Result{}, fmt.Errorf("fork: %w", err)
	}
	defer func() { _ = engine.Terminate(sandboxID) }()

	cc, err := connectGRPCWithRetry(res.VsockPath)
	if err != nil {
		return benchstat.Result{}, err
	}
	defer func() { _ = cc.Close() }()

	ctrl := internalv1.NewControlClient(cc)

	// One mandatory connection-establishing Ping before warmup, matching the
	// JSON exec-rt's connection-establishment exec. This pays the HTTP/2
	// handshake, header compression bootstrap, and the first RPC dispatch.
	if _, err := ctrl.Ping(context.Background(), &internalv1.PingRequest{}); err != nil {
		return benchstat.Result{}, fmt.Errorf("connection-establishment ping: %w", err)
	}
	for i := 0; i < cfg.warmup; i++ {
		if _, err := ctrl.Ping(context.Background(), &internalv1.PingRequest{}); err != nil {
			return benchstat.Result{}, fmt.Errorf("warmup ping %d: %w", i, err)
		}
	}

	samples := make([]time.Duration, 0, cfg.iterations)
	for i := 0; i < cfg.iterations; i++ {
		t0 := time.Now()
		if _, err := ctrl.Ping(context.Background(), &internalv1.PingRequest{}); err != nil {
			return benchstat.Result{}, fmt.Errorf("ping iteration %d: %w", i, err)
		}
		samples = append(samples, time.Since(t0))
	}

	return benchstat.Result{Name: "grpc_ping_round_trip", Unit: "ms", Summary: benchstat.Summarize(samples)}, nil
}

// connectGRPCWithRetry dials the fork's gRPC vsock port (AgentGRPCPort 53),
// retrying briefly because the guest agent needs a moment to start listening
// after a restore. It returns a live *grpc.ClientConn after verifying the
// connection with a single Control.Ping (which also pays the HTTP/2 handshake
// so the first measured call is not cold).
//
// The Rust production guest agent serves the gRPC runtime protocol on this port
// (AgentGRPCPort 53); it is the only runtime transport since #310.
func connectGRPCWithRetry(vsockPath string) (*grpc.ClientConn, error) {
	const attempts = 50
	var lastErr error
	for i := 0; i < attempts; i++ {
		// DialGRPCConn dials the Firecracker vsock UDS and performs the
		// "CONNECT 53\n" / "OK\n" preamble, returning the raw net.Conn
		// ready for HTTP/2 framing.
		conn, dialErr := vsock.DialGRPCConn(vsockPath, vsock.AgentGRPCPort)
		if dialErr != nil {
			lastErr = dialErr
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// DialGRPCOverConn wraps the net.Conn into a *grpc.ClientConn.
		// The dialer callback returns the already-dialed conn the first time it
		// is called; for the bench a single transport connection suffices and we
		// never reconnect.
		capturedConn := conn
		var dialOnce sync.Once
		cc, wrapErr := vsock.DialGRPCOverConn(func() (net.Conn, error) {
			var c net.Conn
			dialOnce.Do(func() { c = capturedConn })
			if c == nil {
				return nil, fmt.Errorf("bench grpc dialer: reconnect not supported")
			}
			return c, nil
		})
		if wrapErr != nil {
			conn.Close()
			lastErr = wrapErr
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// Verify the channel with a Ping before returning; this also pays the
		// HTTP/2 handshake so the first measured call is not cold.
		ctrl := internalv1.NewControlClient(cc)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, pingErr := ctrl.Ping(ctx, &internalv1.PingRequest{})
		cancel()
		if pingErr != nil {
			cc.Close()
			lastErr = pingErr
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return cc, nil
	}
	return nil, fmt.Errorf("grpc connect vsock %s after %d attempts: %w", vsockPath, attempts, lastErr)
}

// oneForkExecGRPC forks one sandbox, dials the guest's gRPC port, calls
// Control.Ping to mark first-response, then terminates. The measurement
// boundary mirrors oneForkExec: clock starts immediately before Fork and
// stops the instant the Ping response is in. Teardown runs after the elapsed
// value is captured and is NOT counted in the returned duration.
func oneForkExecGRPC(engine *fork.Engine, template, sandboxID string) (time.Duration, error) {
	t0 := time.Now()
	res, err := engine.Fork(template, sandboxID, fork.ForkOpts{})
	if err != nil {
		return 0, fmt.Errorf("fork: %w", err)
	}
	cleanup := func() { _ = engine.Terminate(sandboxID) }

	cc, err := connectGRPCWithRetry(res.VsockPath)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("grpc connect: %w", err)
	}

	elapsed := time.Since(t0) // clock stops here: Ping has returned inside connectGRPCWithRetry
	cc.Close()
	cleanup()
	return elapsed, nil
}
