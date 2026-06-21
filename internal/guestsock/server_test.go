package guestsock

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

// envFrom builds an EnvLookup from a map.
func envFrom(m map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// TestHandleInfoReadsOwnIdentity proves the read-own-identity call assembles
// the sandbox's identity and budget from the delivered env: names and numbers
// only, never a secret.
func TestHandleInfoReadsOwnIdentity(t *testing.T) {
	h := Handler{Env: envFrom(map[string]string{
		"MITOS_SANDBOX_ID":        "heartbeat-7f3a",
		"MITOS_CLAIM_NAME":        "claim-1",
		"MITOS_POOL_NAME":         "python-agent",
		"MITOS_WORKSPACE_NAME":    "proj-x",
		"MITOS_BUDGET_MAX_FORKS":  "5",
		"MITOS_BUDGET_FORKS_USED": "2",
		"ANTHROPIC_API_KEY":       "sk-secret",
	})}

	resp := h.Handle(Request{Type: TypeInfo})
	if !resp.OK || resp.Info == nil {
		t.Fatalf("info call failed: %+v", resp)
	}
	id := resp.Info.Identity
	if id.SandboxID != "heartbeat-7f3a" || id.Claim != "claim-1" ||
		id.Pool != "python-agent" || id.Workspace != "proj-x" {
		t.Fatalf("identity not assembled from env: %+v", id)
	}
	if resp.Info.Budget.MaxForks != 5 || resp.Info.Budget.ForksUsed != 2 {
		t.Fatalf("budget not assembled from env: %+v", resp.Info.Budget)
	}
	// No secret value ever appears in the response envelope.
	b, _ := json.Marshal(resp)
	if want := "sk-secret"; containsSecret(string(b), want) {
		t.Fatalf("self-service response must not carry a secret value: %s", b)
	}
}

func containsSecret(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestHandleInfoEmptyEnv tolerates a sandbox the host delivered no identity for
// (e.g. an ephemeral standalone sandbox): empty fields, never a panic.
func TestHandleInfoEmptyEnv(t *testing.T) {
	h := Handler{Env: envFrom(map[string]string{})}
	resp := h.Handle(Request{Type: TypeInfo})
	if !resp.OK || resp.Info == nil {
		t.Fatalf("info call must succeed with no env: %+v", resp)
	}
	if resp.Info.Identity.SandboxID != "" {
		t.Fatalf("expected empty identity, got %+v", resp.Info.Identity)
	}
}

// TestHandleForkNotEnabled proves the fork call returns an LLM-legible
// not-enabled error when no Forker is wired (the current state: budget-gated
// self-fork is continuation, issue #25), so the SDK surface is real and the
// escalation path is named.
func TestHandleForkNotEnabled(t *testing.T) {
	h := Handler{Env: envFrom(nil)}
	resp := h.Handle(Request{Type: TypeFork, Fork: &ForkRequest{N: 3}})
	if resp.OK {
		t.Fatal("fork must report not-enabled until issue #25 wires it")
	}
	if resp.Error == "" {
		t.Fatal("a not-enabled fork must carry a one-line remediation, never a bare failure")
	}
}

// TestHandleForkDelegates proves a wired Forker is called with the request and
// its result is returned.
func TestHandleForkDelegates(t *testing.T) {
	h := Handler{
		Env: envFrom(nil),
		Fork: func(req ForkRequest) (ForkResponse, error) {
			if req.N != 2 {
				t.Fatalf("forker got n=%d want 2", req.N)
			}
			return ForkResponse{Children: []string{"sb-a", "sb-b"}}, nil
		},
	}
	resp := h.Handle(Request{Type: TypeFork, Fork: &ForkRequest{N: 2}})
	if !resp.OK || resp.Fork == nil || len(resp.Fork.Children) != 2 {
		t.Fatalf("fork delegation failed: %+v", resp)
	}
}

// TestHandleUnknownType fails closed on an unknown request type.
func TestHandleUnknownType(t *testing.T) {
	h := Handler{}
	resp := h.Handle(Request{Type: "delete-everything"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("unknown type must fail with an error: %+v", resp)
	}
}

// TestServeOverUnixSocket exercises the full wire path: a Handler serving on a
// real unix socket, a client dialing it and reading the newline-delimited JSON
// response. This is the protocol the mitos.guest SDK speaks.
func TestServeOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mitos.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	h := Handler{Env: envFrom(map[string]string{"MITOS_SANDBOX_ID": "sb-wire"})}
	go func() { _ = h.Serve(lis) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req, _ := json.Marshal(Request{Type: TypeInfo})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Info == nil || resp.Info.Identity.SandboxID != "sb-wire" {
		t.Fatalf("wire round-trip failed: %+v", resp)
	}
}
