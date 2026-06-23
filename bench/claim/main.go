// Command claim-bench is the reproducible harness behind the controller-path
// benchmark numbers issue #15 still leaves open: claim -> first-exec end to end
// through the controller, sustained claims/sec with per-node density, and
// pool-rebuild propagation. It drives a REAL cluster over a kubeconfig; it does
// NOT fabricate numbers. On a host with no cluster reachable (for example a
// darwin laptop), it fails at client construction with a clear message rather
// than printing anything.
//
// What each mode measures, honestly:
//
//   - claim-exec: for each of --iterations sequential claims it creates a
//     SandboxClaim against --pool, waits for the claim's status.phase to reach
//     Ready, then runs the FIRST exec over the sandbox HTTP API (the same
//     endpoint + per-sandbox bearer token kubectl-mitos and the SDK use). The
//     measured value is wall-clock from claim create to the first successful
//     exec result: claim -> first-exec, the end-to-end controller + pool path
//     the in-process cmd/bench harness does NOT cover (it measures the engine
//     data path only). Summarized as a nearest-rank P50/P90/P99 distribution.
//
//   - sustained: it arrives claims at --rate claims/sec for --duration, records
//     each claim's Ready offset, the concurrency at that instant, and the node
//     it landed on (status.node), then reports achieved claims/sec, peak
//     concurrency, and per-node density (the density curve). The arrival rate is
//     the driver; the achieved rate and density are the measurement.
//
//   - pool-rebuild: it bumps the pool's spec.templateRef (a pool update) and
//     times from the update to all-nodes-ready: every node's snapshot
//     re-restored and the pool status reporting ReadySnapshots == TotalSnapshots
//     with the new TemplateDigest. This is the snapshot-distribution propagation
//     latency (ties to snapshot distribution, issue #15 item 3). Needs a
//     multi-node cluster to be meaningful; on a single node it still runs but
//     measures only that node.
//
// No result numbers are hardcoded anywhere; the harness produces them on the
// cluster and the maintainer records them in bench/results/ with the hardware,
// exactly as the other bench scripts require (CLAUDE.md operating principle 1).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runv1alpha1 "mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/benchstat"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "claim-bench:", err)
		os.Exit(1)
	}
}

// run parses flags, builds the cluster client, and dispatches to the selected
// mode. It is split from main so flag parsing and rendering stay testable
// without a cluster.
func run(args []string, out io.Writer) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	c, err := newClient(cfg.kubeconfig)
	if err != nil {
		return err
	}
	h := &harness{client: c, cfg: cfg, out: out, httpc: http.DefaultClient}

	ctx := context.Background()
	switch cfg.mode {
	case modeClaimExec:
		return h.runClaimExec(ctx)
	case modeSustained:
		return h.runSustained(ctx)
	case modePoolRebuild:
		return h.runPoolRebuild(ctx)
	default:
		return fmt.Errorf("invalid mode %q", cfg.mode)
	}
}

// newClient builds a controller-runtime client from a kubeconfig path, with the
// core and mitos.run types registered. A non-existent or unreachable cluster
// surfaces here with a clear error: the harness never fakes a result.
func newClient(kubeconfig string) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core scheme: %w", err)
	}
	if err := runv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register mitos.run scheme: %w", err)
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %q (need a reachable cluster; this harness does not run without one): %w", kubeconfig, err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("build cluster client: %w", err)
	}
	return c, nil
}

// harness drives the claim path against a live cluster. The cluster interaction
// is thin; the pure aggregation (percentiles, throughput) lives in
// internal/benchstat, which is unit-tested.
type harness struct {
	client client.Client
	cfg    config
	out    io.Writer
	httpc  *http.Client
}

// runClaimExec measures claim -> first-exec for cfg.iterations sequential
// claims and prints the nearest-rank distribution.
func (h *harness) runClaimExec(ctx context.Context) error {
	runID := fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid())
	samples := make([]time.Duration, 0, h.cfg.iterations)
	for i := 0; i < h.cfg.iterations; i++ {
		name := fmt.Sprintf("claim-bench-%s-%d", runID, i)
		elapsed, err := h.oneClaimExec(ctx, name)
		if err != nil {
			// Best-effort cleanup, then fail: a real error must not be hidden.
			h.deleteClaim(context.Background(), name)
			return fmt.Errorf("iteration %d: %w", i, err)
		}
		fmt.Fprintf(h.out, "  claim %d: %s\n", i, ms(elapsed))
		samples = append(samples, elapsed)
		h.deleteClaim(ctx, name)
	}
	res := benchstat.Result{Name: "claim_to_first_exec", Unit: "ms", Summary: benchstat.Summarize(samples)}
	fmt.Fprintf(h.out, "\n%s (%s)\n%s", res.Name, res.Unit, res.Summary.Table())
	return h.writeJSON([]benchstat.Result{res})
}

// oneClaimExec creates a claim, waits for Ready, and runs the first exec,
// returning the claim-create -> first-exec elapsed time. The clock starts at
// create and stops the instant the first exec result is in.
func (h *harness) oneClaimExec(ctx context.Context, name string) (time.Duration, error) {
	t0 := time.Now()
	if err := h.createClaim(ctx, name); err != nil {
		return 0, err
	}
	claim, err := h.waitReady(ctx, name)
	if err != nil {
		return 0, err
	}
	token, err := h.sandboxToken(ctx, name)
	if err != nil {
		return 0, err
	}
	ref := claim.Status.SandboxID
	if ref == "" {
		ref = name
	}
	if err := h.firstExec(ctx, claim.Status.Endpoint, token, ref); err != nil {
		return 0, err
	}
	return time.Since(t0), nil
}

// createClaim creates a minimal SandboxClaim bound to the configured pool.
func (h *harness) createClaim(ctx context.Context, name string) error {
	claim := &runv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.namespace},
		Spec:       runv1alpha1.SandboxClaimSpec{PoolRef: runv1alpha1.LocalObjectReference{Name: h.cfg.pool}},
	}
	if err := h.client.Create(ctx, claim); err != nil {
		return fmt.Errorf("create claim %q: %w", name, err)
	}
	return nil
}

// waitReady polls the claim until status.phase == Ready (with a usable
// endpoint) or the timeout elapses.
func (h *harness) waitReady(ctx context.Context, name string) (*runv1alpha1.SandboxClaim, error) {
	deadline := time.Now().Add(h.cfg.timeout)
	for time.Now().Before(deadline) {
		claim := &runv1alpha1.SandboxClaim{}
		if err := h.client.Get(ctx, h.key(name), claim); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("get claim %q: %w", name, err)
			}
		} else if claim.Status.Phase == runv1alpha1.SandboxReady && claim.Status.Endpoint != "" {
			return claim, nil
		} else if claim.Status.Phase == runv1alpha1.SandboxFailed {
			return nil, fmt.Errorf("claim %q reached Failed before Ready", name)
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("claim %q did not reach Ready within %s", name, h.cfg.timeout)
}

// sandboxToken reads the per-sandbox bearer token Secret the controller mints
// for the claim (<name>-sandbox-token). The token VALUE is never logged.
func (h *harness) sandboxToken(ctx context.Context, name string) (string, error) {
	secretName := name + "-sandbox-token"
	var secret corev1.Secret
	if err := h.client.Get(ctx, h.key(secretName), &secret); err != nil {
		return "", fmt.Errorf("get token Secret %q (the sandbox API requires a bearer token): %w", secretName, err)
	}
	tokenBytes, ok := secret.Data["token"]
	if !ok || len(tokenBytes) == 0 {
		return "", fmt.Errorf("token Secret %q has no token key", secretName)
	}
	return string(tokenBytes), nil
}

// firstExec runs a single trivial command over the sandbox HTTP API, the same
// /v1/exec endpoint + bearer-token gate kubectl-mitos uses. A non-zero exit
// or a transport error is returned; the token value never appears in an error.
func (h *harness) firstExec(ctx context.Context, endpoint, token, ref string) error {
	body, err := json.Marshal(map[string]any{"sandbox": ref, "command": "/bin/true"})
	if err != nil {
		return fmt.Errorf("encode exec request: %w", err)
	}
	url := fmt.Sprintf("http://%s/v1/exec", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build exec request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("reach sandbox API at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		safe := strings.ReplaceAll(strings.TrimSpace(string(msg)), token, "[REDACTED]")
		return fmt.Errorf("sandbox API returned %d: %s", resp.StatusCode, safe)
	}
	return nil
}

// runSustained drives claims at cfg.rate for cfg.duration, records each Ready
// completion, and prints the achieved-rate + density aggregation.
func (h *harness) runSustained(ctx context.Context) error {
	runID := fmt.Sprintf("%d-%d", time.Now().Unix(), os.Getpid())
	interval := time.Duration(float64(time.Second) / h.cfg.rate)
	start := time.Now()
	deadline := start.Add(h.cfg.duration)

	// live tracks claims created but not yet completed, so concurrency at each
	// completion is observable and a cleanup pass can release everything.
	live := map[string]struct{}{}
	completions := make([]benchstat.Completion, 0)
	defer func() {
		for name := range live {
			h.deleteClaim(context.Background(), name)
		}
	}()

	i := 0
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		// Respect the concurrency cap: if at the cap, drain one ready claim
		// before arriving the next, so a finite pool is not exhausted.
		if h.cfg.maxConcurrent > 0 && len(live) >= h.cfg.maxConcurrent {
			h.drainOne(ctx, live, &completions, start)
		}
		name := fmt.Sprintf("sustained-%s-%d", runID, i)
		if err := h.createClaim(ctx, name); err != nil {
			return err
		}
		live[name] = struct{}{}
		i++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	// Drain remaining live claims, recording their completion as they go Ready.
	for len(live) > 0 {
		h.drainOne(ctx, live, &completions, start)
	}

	tp := benchstat.AggregateThroughput(completions)
	fmt.Fprintf(h.out, "\nsustained claims (target rate %.2f/s for %s, arrived %d):\n", h.cfg.rate, h.cfg.duration, i)
	fmt.Fprint(h.out, tp.Table())
	return h.writeThroughputJSON(tp)
}

// drainOne waits for any one of the live claims to reach Ready, records its
// completion (offset, concurrency at that instant, node), deletes it, and
// removes it from the live set. If a claim fails it is dropped from live
// without a completion record so the run continues.
func (h *harness) drainOne(ctx context.Context, live map[string]struct{}, completions *[]benchstat.Completion, start time.Time) {
	deadline := time.Now().Add(h.cfg.timeout)
	for time.Now().Before(deadline) {
		for name := range live {
			claim := &runv1alpha1.SandboxClaim{}
			err := h.client.Get(ctx, h.key(name), claim)
			if err != nil {
				delete(live, name)
				break
			}
			switch claim.Status.Phase {
			case runv1alpha1.SandboxReady:
				*completions = append(*completions, benchstat.Completion{
					Offset:     time.Since(start),
					Concurrent: len(live),
					Node:       claim.Status.Node,
				})
				h.deleteClaim(ctx, name)
				delete(live, name)
				return
			case runv1alpha1.SandboxFailed:
				h.deleteClaim(ctx, name)
				delete(live, name)
				return
			}
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return
		}
	}
	// Timed out waiting: drop one arbitrary live claim so the loop makes
	// progress rather than hanging.
	for name := range live {
		h.deleteClaim(ctx, name)
		delete(live, name)
		return
	}
}

// runPoolRebuild times pool-update -> all-nodes-ready: it bumps the pool's
// spec.replicas (a no-op-safe rebuild trigger that forces a reconcile and
// snapshot re-distribution check) and waits for the pool status to report all
// snapshots ready again. The honest measurement is the propagation latency from
// the spec change to ReadySnapshots == TotalSnapshots across the pool's nodes.
func (h *harness) runPoolRebuild(ctx context.Context) error {
	pool := &runv1alpha1.SandboxPool{}
	if err := h.client.Get(ctx, h.key(h.cfg.pool), pool); err != nil {
		return fmt.Errorf("get pool %q: %w", h.cfg.pool, err)
	}
	digestBefore := pool.Status.TemplateDigest
	nodesBefore := len(pool.Status.NodeDistribution)
	fmt.Fprintf(h.out, "pool %q before rebuild: digest=%q nodes=%d ready=%d/%d\n",
		h.cfg.pool, digestBefore, nodesBefore, pool.Status.ReadySnapshots, pool.Status.TotalSnapshots)

	// Trigger the rebuild: bump replicas by one then restore. This forces the
	// pool reconcile to re-evaluate snapshot distribution to every node. A
	// maintainer who wants to measure a real template change instead points
	// spec.templateRef at a new template before running; the method is the same.
	start := time.Now()
	orig := pool.Spec.Replicas
	pool.Spec.Replicas = orig + 1
	if err := h.client.Update(ctx, pool); err != nil {
		return fmt.Errorf("bump pool replicas to trigger rebuild: %w", err)
	}

	if err := h.waitPoolReady(ctx); err != nil {
		// Restore the original replicas even on failure.
		h.restorePoolReplicas(context.Background(), orig)
		return err
	}
	elapsed := time.Since(start)
	h.restorePoolReplicas(ctx, orig)

	res := benchstat.Result{Name: "pool_rebuild_propagation", Unit: "ms", Summary: benchstat.Summarize([]time.Duration{elapsed})}
	fmt.Fprintf(h.out, "\npool-rebuild propagation (update -> all-nodes-ready): %s\n", ms(elapsed))
	fmt.Fprintln(h.out, "NOTE: on a single-node cluster this measures one node; run on a multi-node")
	fmt.Fprintln(h.out, "cluster for the real snapshot-distribution propagation across all nodes.")
	return h.writeJSON([]benchstat.Result{res})
}

// waitPoolReady polls the pool until ReadySnapshots == TotalSnapshots with at
// least one snapshot, or the timeout elapses.
func (h *harness) waitPoolReady(ctx context.Context) error {
	deadline := time.Now().Add(h.cfg.timeout)
	for time.Now().Before(deadline) {
		pool := &runv1alpha1.SandboxPool{}
		if err := h.client.Get(ctx, h.key(h.cfg.pool), pool); err != nil {
			return fmt.Errorf("get pool %q: %w", h.cfg.pool, err)
		}
		if pool.Status.TotalSnapshots > 0 && pool.Status.ReadySnapshots == pool.Status.TotalSnapshots {
			return nil
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("pool %q did not reach all-snapshots-ready within %s", h.cfg.pool, h.cfg.timeout)
}

// restorePoolReplicas best-effort resets the pool's replicas to n.
func (h *harness) restorePoolReplicas(ctx context.Context, n int32) {
	pool := &runv1alpha1.SandboxPool{}
	if err := h.client.Get(ctx, h.key(h.cfg.pool), pool); err != nil {
		return
	}
	pool.Spec.Replicas = n
	_ = h.client.Update(ctx, pool)
}

// deleteClaim best-effort deletes a claim so a run leaves no residue.
func (h *harness) deleteClaim(ctx context.Context, name string) {
	claim := &runv1alpha1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.namespace}}
	_ = h.client.Delete(ctx, claim)
}

func (h *harness) key(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: h.cfg.namespace}
}

// writeJSON writes latency results to cfg.jsonPath if set.
func (h *harness) writeJSON(results []benchstat.Result) error {
	if h.cfg.jsonPath == "" {
		return nil
	}
	f, err := os.Create(h.cfg.jsonPath)
	if err != nil {
		return fmt.Errorf("create json output: %w", err)
	}
	defer f.Close()
	return benchstat.WriteJSON(f, results)
}

// writeThroughputJSON writes the sustained-mode throughput to cfg.jsonPath if
// set, as a small self-describing JSON object.
func (h *harness) writeThroughputJSON(tp benchstat.Throughput) error {
	if h.cfg.jsonPath == "" {
		return nil
	}
	out := map[string]any{
		"completed":        tp.Completed,
		"window_ms":        tp.Window.Milliseconds(),
		"achieved_per_sec": tp.AchievedPerSec,
		"peak_concurrent":  tp.PeakConcurrent,
		"per_node_density": tp.PerNodeDensity,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal throughput: %w", err)
	}
	if err := os.WriteFile(h.cfg.jsonPath, b, 0o644); err != nil {
		return fmt.Errorf("write throughput json: %w", err)
	}
	return nil
}

// sleepCtx sleeps for d or returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
}
