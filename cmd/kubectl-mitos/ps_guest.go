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
	"text/tabwriter"
	"time"
)

// This file is the kubectl mitos ps consumer of the Layer 3 guest telemetry
// bridge (issue #164): with --processes, ps fetches the REAL in-guest process
// table from forkd's /v1/vitals endpoint (which asks the guest agent over vsock)
// instead of listing fork Sandbox objects. When the guest is unreachable the
// caller falls back to the object listing, so ps never renders a fabricated
// table.

// guestProcess mirrors one row of the forkd vitals process table.
type guestProcess struct {
	PID        int    `json:"pid"`
	Comm       string `json:"comm"`
	State      string `json:"state"`
	CPUJiffies uint64 `json:"cpu_jiffies"`
	RSSKB      uint64 `json:"rss_kb"`
}

// guestVitals mirrors the forkd vitals snapshot the ps consumer reads.
type guestVitals struct {
	StealFraction      float64        `json:"steal_fraction"`
	MemTotalKB         uint64         `json:"mem_total_kb"`
	MemUsedKB          uint64         `json:"mem_used_kb"`
	BalloonReclaimedKB uint64         `json:"balloon_reclaimed_kb"`
	Processes          []guestProcess `json:"processes"`
}

// labeledVitals mirrors the forkd /v1/vitals LabeledVitals response: the guest
// snapshot plus the host's claim/pool/workspace labels.
type labeledVitals struct {
	Claim     string      `json:"claim"`
	Pool      string      `json:"pool"`
	Workspace string      `json:"workspace"`
	Namespace string      `json:"namespace"`
	Vitals    guestVitals `json:"vitals"`
}

// fetchGuestVitals GETs the labeled guest telemetry snapshot from forkd's
// /v1/vitals endpoint for one sandbox, sending the per-sandbox bearer token (the
// same gate exec uses; auth is never bypassed). Any error (unreachable, non-2xx,
// decode) is returned so the caller falls back to the object listing. The token
// value never appears in a returned error.
func fetchGuestVitals(ctx context.Context, httpc *http.Client, endpoint, token, ref string) (labeledVitals, error) {
	body, err := json.Marshal(map[string]any{"sandbox": ref})
	if err != nil {
		return labeledVitals{}, fmt.Errorf("encode vitals request: %w", err)
	}
	url := fmt.Sprintf("http://%s/v1/vitals", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return labeledVitals{}, fmt.Errorf("build vitals request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpc.Do(req)
	if err != nil {
		return labeledVitals{}, fmt.Errorf("reach sandbox API at %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		safe := strings.ReplaceAll(strings.TrimSpace(string(msg)), token, "[REDACTED]")
		return labeledVitals{}, fmt.Errorf("sandbox API returned %d: %s", resp.StatusCode, safe)
	}
	var v labeledVitals
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return labeledVitals{}, fmt.Errorf("decode vitals response: %w", err)
	}
	return v, nil
}

// runPsProcesses fetches and prints the REAL in-guest process table for one
// sandbox via forkd's /v1/vitals endpoint. When the guest is unreachable (the
// sandbox is not running, the endpoint is down, or KVM is absent so no guest is
// behind it) it falls back to the fork sandbox listing for that sandbox, so the
// command always shows something honest and never a fabricated table.
func runPsProcesses(namespace, name string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref, endpoint, token, err := resolveSandboxAuth(ctx, c, namespace, name)
	if err == nil && endpoint != "" {
		v, ferr := fetchGuestVitals(ctx, http.DefaultClient, endpoint, token, ref)
		if ferr == nil {
			fmt.Print(renderGuestProcesses(v))
			return nil
		}
		fmt.Fprintf(os.Stderr, "note: guest telemetry unavailable (%v); falling back to fork sandbox listing\n", ferr)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "note: %v; falling back to fork sandbox listing\n", err)
	}
	return runPs(namespace, false, name)
}

// renderGuestProcesses formats the real in-guest process table as a column table
// with the claim/pool/workspace labels in a header. RSS is shown in MiB.
func renderGuestProcesses(v labeledVitals) string {
	var b strings.Builder
	fmt.Fprintf(&b, "NAMESPACE %s  CLAIM %s  POOL %s  WORKSPACE %s\n",
		orDash(v.Namespace), orDash(v.Claim), orDash(v.Pool), orDash(v.Workspace))
	fmt.Fprintf(&b, "STEAL %.1f%%  MEM %d/%d MiB used  BALLOON %d MiB reclaimed\n\n",
		v.Vitals.StealFraction*100,
		v.Vitals.MemUsedKB/1024, v.Vitals.MemTotalKB/1024,
		v.Vitals.BalloonReclaimedKB/1024)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PID\tCOMMAND\tSTATE\tCPU(jiffies)\tRSS(MiB)")
	for _, p := range v.Vitals.Processes {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%d\n", p.PID, p.Comm, p.State, p.CPUJiffies, p.RSSKB/1024)
	}
	_ = tw.Flush()
	return b.String()
}

// orDash renders an empty label as a dash so an unlabeled field is visibly
// absent rather than blank.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
