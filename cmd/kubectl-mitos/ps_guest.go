package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	connect "connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// This file is the kubectl mitos ps consumer of the Layer 3 guest telemetry
// bridge (issue #164): with --processes, ps fetches the REAL in-guest vitals from
// a sandbox's guest agent over the Connect sandbox.v1.Sandbox/Vitals and
// /Processes RPCs instead of listing fork Sandbox objects. When the guest is
// unreachable the caller falls back to the object listing, so ps never renders a
// fabricated table.

// guestProcess mirrors one row of a guest vitals process table.
type guestProcess struct {
	PID        int
	Comm       string
	State      string
	CPUJiffies uint64
	RSSKB      uint64
}

// guestVitals mirrors the guest vitals snapshot the ps consumer renders.
type guestVitals struct {
	StealFraction      float64
	MemTotalKB         uint64
	MemUsedKB          uint64
	BalloonReclaimedKB uint64
	Processes          []guestProcess
}

// labeledVitals is the guest snapshot the ps consumer renders, plus the host's
// claim/pool/workspace labels. The numeric sample (steal, memory vs balloon)
// comes from the Connect Vitals RPC and the per-process table from the Connect
// Processes RPC. The claim/pool/workspace labels are not carried by either RPC,
// so they render empty until a future labeled RPC fills them.
type labeledVitals struct {
	Claim     string
	Pool      string
	Workspace string
	Namespace string
	Vitals    guestVitals
}

// fetchGuestVitals builds the guest snapshot from TWO Connect calls on one
// client: the sandbox.v1.Sandbox/Vitals server-streaming RPC supplies the first
// numeric GuestVitals sample (steal, memory vs balloon), and the Processes RPC
// supplies the per-process table. Both carry the per-sandbox bearer token and
// sandbox id on the Authorization and X-Sandbox-Id headers (the same gate exec
// uses; auth is never bypassed). If EITHER call fails (unreachable, rejected
// token, empty stream) an error is returned so the caller falls back to the
// object listing rather than rendering a fabricated table. The token value never
// appears in a returned error.
//
// httpc may be nil, in which case http.DefaultClient is used.
func fetchGuestVitals(ctx context.Context, httpc *http.Client, endpoint, token, ref string) (labeledVitals, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cli := sandboxv1connect.NewSandboxClient(httpc, "http://"+endpoint)

	// setAuth stamps the per-sandbox bearer token and sandbox id on a request so
	// both Connect calls authenticate identically.
	setAuth := func(h http.Header) {
		h.Set("Authorization", "Bearer "+token)
		h.Set("X-Sandbox-Id", ref)
	}

	vreq := connect.NewRequest(&sandboxv1.VitalsRequest{})
	setAuth(vreq.Header())

	stream, err := cli.Vitals(ctx, vreq)
	if err != nil {
		return labeledVitals{}, vitalsConnectError(endpoint, token, err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return labeledVitals{}, vitalsConnectError(endpoint, token, err)
		}
		return labeledVitals{}, fmt.Errorf("guest vitals unavailable at %s: no sample", endpoint)
	}

	sample := stream.Msg()
	var out labeledVitals
	// CpuStealPercent is [0,100]; StealFraction is [0,1].
	out.Vitals.StealFraction = sample.GetCpuStealPercent() / 100.0
	out.Vitals.MemTotalKB = uint64(sample.GetMemTotalBytes()) / 1024
	out.Vitals.MemUsedKB = uint64(sample.GetMemUsedBytes()) / 1024
	out.Vitals.BalloonReclaimedKB = uint64(sample.GetMemBalloonBytes()) / 1024

	// The per-process table comes from the unary Processes RPC. A failure here is
	// fatal to the snapshot so the caller degrades to the object listing rather
	// than rendering vitals with a silently empty table. CPUJiffies has no field
	// on the Connect ProcessInfo (it was JSON-only on the legacy path), so it
	// stays 0.
	preq := connect.NewRequest(&sandboxv1.ProcessesRequest{})
	setAuth(preq.Header())
	pl, perr := cli.Processes(ctx, preq)
	if perr != nil {
		return labeledVitals{}, vitalsConnectError(endpoint, token, perr)
	}
	for _, p := range pl.Msg.GetProcesses() {
		out.Vitals.Processes = append(out.Vitals.Processes, guestProcess{
			PID:   int(p.GetPid()),
			Comm:  p.GetCommand(),
			State: p.GetState(),
			RSSKB: uint64(p.GetRssBytes()) / 1024,
		})
	}
	return out, nil
}

// vitalsConnectError wraps a Connect Vitals or Processes failure with the
// endpoint, redacting the token from any cause so it never reaches an error
// string a caller may log.
func vitalsConnectError(endpoint, token string, err error) error {
	safe := err
	if token != "" {
		safe = errors.New(strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
	}
	return fmt.Errorf("reach sandbox API at %s: %w", endpoint, safe)
}

// runPsProcesses fetches and prints the REAL in-guest vitals for one sandbox via
// the Connect sandbox.v1.Sandbox/Vitals RPC. When the guest is unreachable (the
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
