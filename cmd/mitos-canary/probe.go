package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"mitos.run/mitos/internal/agentcli"
)

// reapTimeout bounds the best-effort Terminate that cleans up each probe's
// sandbox. It uses its own context so a cancelled cycle context (deadline hit
// mid-exec) still gets a chance to reap the sandbox and not leak it.
const reapTimeout = 30 * time.Second

// sandboxAPI is the minimal slice of the sandbox backend the canary probe
// needs. *agentcli.HostedBackend satisfies it directly; the unit test supplies
// a fake so the probe logic is exercised without a live cluster.
type sandboxAPI interface {
	Create(ctx context.Context, pool string) (string, error)
	Exec(ctx context.Context, sandboxID, command string, timeoutSec int) (agentcli.ExecResult, error)
	Terminate(ctx context.Context, sandboxID string) error
}

// probePhase names one step of a probe cycle. It doubles as the {phase} metric
// label value, so the names are stable and lowercase.
type probePhase string

const (
	phaseCreate    probePhase = "create"
	phaseExec      probePhase = "exec"
	phaseVerify    probePhase = "verify"
	phaseTerminate probePhase = "terminate"
	phaseCycle     probePhase = "cycle"
)

// probeResult is the outcome of one full probe cycle. Phase is the step that
// decided the outcome: the failing step on failure, or phaseVerify on success
// (the last step of the user-facing path). Err is nil on success and always
// carries a redacted, actionable message on failure.
type probeResult struct {
	Phase probePhase
	OK    bool
	Err   error
}

// metrics holds the canary's Prometheus collectors. Constructed against an
// explicit registry so tests get an isolated one and never touch the global
// default registerer.
type metrics struct {
	probeTotal    *prometheus.CounterVec   // {phase,result} attempts per phase
	probeDuration *prometheus.HistogramVec // {phase} wall-clock per phase
	cycleDuration prometheus.Histogram     // full create->verify->terminate cycle
	lastSuccess   prometheus.Gauge         // unix time of the last fully-green cycle
	up            prometheus.Gauge         // 1 if the last cycle passed, else 0
	consecFail    prometheus.Gauge         // consecutive failed cycles (0 when green)
}

// newMetrics registers the canary collectors on reg and returns them.
func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		probeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mitos_canary_probe_total",
			Help: "Canary probe attempts by phase and result (success|failure).",
		}, []string{"phase", "result"}),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mitos_canary_probe_duration_seconds",
			Help:    "Wall-clock duration of each canary probe phase.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}, []string{"phase"}),
		cycleDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "mitos_canary_cycle_duration_seconds",
			Help:    "Wall-clock duration of a full canary cycle (create, exec, verify, terminate).",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mitos_canary_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last fully successful canary cycle.",
		}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mitos_canary_up",
			Help: "1 if the most recent canary cycle passed end to end, else 0.",
		}),
		consecFail: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mitos_canary_consecutive_failures",
			Help: "Number of consecutive failed canary cycles; 0 when the last cycle was green.",
		}),
	}
	reg.MustRegister(m.probeTotal, m.probeDuration, m.cycleDuration, m.lastSuccess, m.up, m.consecFail)
	return m
}

// resultLabel maps success to the stable metric label values.
func resultLabel(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}

// observe runs fn, records its duration under the phase histogram and bumps the
// {phase,result} counter, then returns fn's value and error. Generic so each
// phase keeps its natural return type.
func observe[T any](m *metrics, phase probePhase, fn func() (T, error)) (T, error) {
	start := time.Now()
	v, err := fn()
	m.probeDuration.WithLabelValues(string(phase)).Observe(time.Since(start).Seconds())
	m.probeTotal.WithLabelValues(string(phase), resultLabel(err == nil)).Inc()
	return v, err
}

// runProbe executes one full canary cycle against api and records every metric
// for it: create a sandbox from pool, exec `echo <nonce>`, verify the nonce
// round-tripped with a zero exit code, then terminate. Terminate always runs
// once a sandbox exists, even when an earlier phase failed, so the canary never
// leaks sandboxes. The returned probeResult reflects the user-facing path
// (create, exec, verify); a terminate-only failure is recorded in metrics but
// does not by itself mark the cycle down, since the fork path still worked.
func runProbe(ctx context.Context, api sandboxAPI, m *metrics, pool, nonce string, execTimeoutSec int) probeResult {
	cycleStart := time.Now()
	res := doProbe(ctx, api, m, pool, nonce, execTimeoutSec)

	m.cycleDuration.Observe(time.Since(cycleStart).Seconds())
	m.probeTotal.WithLabelValues(string(phaseCycle), resultLabel(res.OK)).Inc()
	if res.OK {
		m.up.Set(1)
		m.lastSuccess.SetToCurrentTime()
	} else {
		m.up.Set(0)
	}
	return res
}

// doProbe is the cycle body without the cycle-level metric bookkeeping, split
// out so runProbe stays readable.
func doProbe(ctx context.Context, api sandboxAPI, m *metrics, pool, nonce string, execTimeoutSec int) probeResult {
	id, err := observe(m, phaseCreate, func() (string, error) {
		return api.Create(ctx, pool)
	})
	if err != nil {
		return probeResult{Phase: phaseCreate, OK: false, Err: fmt.Errorf("create sandbox from template %q: %w", pool, err)}
	}
	// Once a sandbox exists, always reap it: a cancelled cycle context must not
	// leave a live sandbox behind.
	defer reap(api, m, id)

	out, err := observe(m, phaseExec, func() (agentcli.ExecResult, error) {
		return api.Exec(ctx, id, "echo "+nonce, execTimeoutSec)
	})
	if err != nil {
		return probeResult{Phase: phaseExec, OK: false, Err: fmt.Errorf("exec in sandbox %s: %w", id, err)}
	}

	// Verify: the nonce we sent must come back, proving the exec actually ran in
	// the guest rather than a proxy returning an empty 200.
	ok := strings.Contains(out.Stdout, nonce) && out.ExitCode == 0
	m.probeTotal.WithLabelValues(string(phaseVerify), resultLabel(ok)).Inc()
	if !ok {
		return probeResult{
			Phase: phaseVerify,
			OK:    false,
			Err:   fmt.Errorf("canary nonce did not round-trip (exit=%d): the sandbox exec path is not returning guest output", out.ExitCode),
		}
	}
	return probeResult{Phase: phaseVerify, OK: true}
}

// reap terminates a probe's sandbox on a fresh, bounded context so cleanup runs
// even when the cycle context was already cancelled. Failures are recorded in
// metrics (a rising terminate-failure counter means leaked sandboxes) but never
// abort the caller.
func reap(api sandboxAPI, m *metrics, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	_, _ = observe(m, phaseTerminate, func() (struct{}, error) {
		return struct{}{}, api.Terminate(ctx, id)
	})
}
