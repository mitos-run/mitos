package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchGuestVitals_RealProcesses verifies the ps --processes path consumes
// the forkd /v1/vitals endpoint and surfaces REAL guest processes (not
// SandboxFork objects). The endpoint is faked; the bearer token must be sent.
func TestFetchGuestVitals_RealProcesses(t *testing.T) {
	var sawAuth, sawSandbox string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		sawSandbox = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"claim":"claim-a","pool":"pool-x","workspace":"ws-1","namespace":"team-ns",
			"vitals":{"steal_fraction":0.1,"mem_total_kb":2048000,"mem_used_kb":1024000,
			"balloon_reclaimed_kb":512000,
			"processes":[{"pid":1,"comm":"agent","state":"S","cpu_jiffies":42,"rss_kb":4096},
			{"pid":99,"comm":"python","state":"R","cpu_jiffies":7,"rss_kb":65536}]}}`))
	}))
	defer srv.Close()

	endpoint := strings.TrimPrefix(srv.URL, "http://")
	v, err := fetchGuestVitals(context.Background(), srv.Client(), endpoint, "tok-123", "sb-1")
	if err != nil {
		t.Fatalf("fetchGuestVitals: %v", err)
	}
	if sawAuth != "Bearer tok-123" {
		t.Errorf("auth header = %q, want Bearer tok-123", sawAuth)
	}
	if !strings.Contains(sawSandbox, "sb-1") {
		t.Errorf("request body = %q, want sandbox sb-1", sawSandbox)
	}
	if v.Claim != "claim-a" {
		t.Errorf("claim = %q, want claim-a", v.Claim)
	}
	if v.Namespace != "team-ns" {
		t.Errorf("namespace = %q, want team-ns", v.Namespace)
	}
	if len(v.Vitals.Processes) != 2 || v.Vitals.Processes[1].Comm != "python" {
		t.Errorf("processes = %+v, want 2 incl python", v.Vitals.Processes)
	}
}

func TestFetchGuestVitals_Unreachable(t *testing.T) {
	// A non-routable endpoint must error so ps falls back to the object listing
	// rather than rendering a fabricated table.
	_, err := fetchGuestVitals(context.Background(), http.DefaultClient, "127.0.0.1:0", "tok", "sb-1")
	if err == nil {
		t.Error("expected error for unreachable endpoint")
	}
}

// TestRenderGuestProcesses formats the real process table; this is the text a
// user sees from `kubectl sandbox ps <name> --processes`.
func TestRenderGuestProcesses(t *testing.T) {
	v := labeledVitals{
		Claim:     "claim-a",
		Namespace: "team-ns",
		Vitals: guestVitals{
			Processes: []guestProcess{
				{PID: 1, Comm: "agent", State: "S", CPUJiffies: 42, RSSKB: 4096},
				{PID: 99, Comm: "python", State: "R", CPUJiffies: 7, RSSKB: 65536},
			},
		},
	}
	out := renderGuestProcesses(v)
	for _, want := range []string{"NAMESPACE", "team-ns", "PID", "COMMAND", "STATE", "RSS", "agent", "python", "99"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
