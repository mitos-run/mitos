package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mitos.run/mitos/internal/vsock"
)

// TestMaxStreamsPerSandboxFlagDefault verifies the standalone server exposes a
// --max-streams-per-sandbox flag defaulting to 16, matching forkd. Before this
// fix the standalone REST path had no flag and never capped streams.
func TestMaxStreamsPerSandboxFlagDefault(t *testing.T) {
	fs := flag.NewFlagSet("sandbox-server", flag.ContinueOnError)
	var maxStreams int
	fs.IntVar(&maxStreams, "max-streams-per-sandbox", 16, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if maxStreams != 16 {
		t.Fatalf("default --max-streams-per-sandbox: got %d want 16 (forkd default)", maxStreams)
	}
}

// TestNewServerPlumbsStreamCap verifies the flag value reaches the server: the
// per-sandbox stream ceiling passed to newServer is applied to the SandboxAPI
// (the value is retained on the server for observability). A parsed flag of 7
// must surface unchanged.
func TestNewServerPlumbsStreamCap(t *testing.T) {
	const want = 7
	s := newServer(t.TempDir(), "", true, want, 86400)
	if s.sandboxAPI == nil {
		t.Fatal("newServer must construct a SandboxAPI")
	}
	if s.maxStreamsPerSandbox != want {
		t.Fatalf("newServer stream cap: got %d want %d", s.maxStreamsPerSandbox, want)
	}
}

// fakeAgent listens on sockPath speaking the Firecracker vsock UDS preamble and
// the JSON agent protocol, recording notify_forked requests. On notify_forked it
// replies OK:true with a NotifyForkedResponse reporting ReseededRNG=reseeded, so
// a test can drive both the reseeded-OK and the un-reseeded fail-closed path. No
// secrets or entropy are ever logged.
func fakeAgent(t *testing.T, sockPath string, reseeded bool) *[]*vsock.NotifyForkedRequest {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	var notifies []*vsock.NotifyForkedRequest
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				// The standalone server reaches this fake over the unix fallback
				// (vsock.ConnectUnix), which sends no "CONNECT <port>" preamble:
				// the JSON request protocol starts immediately.
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					if req.Type == vsock.TypeNotifyForked {
						notifies = append(notifies, req.NotifyForked)
						out, _ := json.Marshal(vsock.Response{
							OK:           true,
							NotifyForked: &vsock.NotifyForkedResponse{ReseededRNG: reseeded},
						})
						if _, err := c.Write(append(out, '\n')); err != nil {
							return
						}
						continue
					}
					out, _ := json.Marshal(vsock.Response{OK: true})
					if _, err := c.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return &notifies
}

// flakyAgent is like fakeAgent but DROPS the connection without replying on the
// first failFirst notify_forked requests it sees (across reconnects), then
// behaves normally (reply OK with ReseededRNG:true). It models the post-restore
// readiness race: after a snapshot restore the guest agent resets its vsock
// listener, so the host's first connection can be stale and NotifyForked sees
// "connection closed". A robust fork path must reconnect and retry. No secrets
// or entropy are ever logged.
func flakyAgent(t *testing.T, sockPath string, failFirst int) *int32 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	var seen int32
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					var req vsock.Request
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					if req.Type == vsock.TypeNotifyForked {
						if atomic.AddInt32(&seen, 1) <= int32(failFirst) {
							// Drop the connection without replying: the caller must
							// reconnect and retry.
							return
						}
						out, _ := json.Marshal(vsock.Response{
							OK:           true,
							NotifyForked: &vsock.NotifyForkedResponse{ReseededRNG: true},
						})
						if _, err := c.Write(append(out, '\n')); err != nil {
							return
						}
						continue
					}
					out, _ := json.Marshal(vsock.Response{OK: true})
					if _, err := c.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return &seen
}

// realServerWithAgent builds a real-mode server whose sandboxAPI dials a fake
// guest agent for sandboxID at the standalone server's fixed vsock path. The
// real-mode handleFork resolves the vsock UDS under dataDir/sandboxes/<id>;
// since the path does not exist on disk, the EnableUnixFallback path the
// standalone server sets routes the dial to the fixed local agent socket.
func realServerWithAgent(t *testing.T, sandboxID string, reseeded bool) (*server, *[]*vsock.NotifyForkedRequest) {
	t.Helper()
	// The standalone server falls back to /tmp/sandbox-agent-52.sock when the
	// per-sandbox vsock path does not exist, so the fake agent listens there.
	sock := fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort)
	_ = os.Remove(sock)
	notifies := fakeAgent(t, sock, reseeded)

	dataDir, err := os.MkdirTemp("/tmp", "sbsrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDir) })

	s := newServer(dataDir, "", false, 16, 86400) // real mode
	s.templates[sandboxID+"-tmpl"] = &templateInfo{ID: sandboxID + "-tmpl", Ready: true}
	return s, notifies
}

func forkRequest(t *testing.T, s *server, id, template string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id, "template": template})
	r := httptest.NewRequest(http.MethodPost, "/v1/fork", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleFork(w, r)
	return w
}

// TestRealModeForkReseedsAndSucceeds proves the standalone server runs the
// reseed handshake on a real-mode fork and serves the sandbox when the guest
// reports ReseededRNG:true.
func TestRealModeForkReseedsAndSucceeds(t *testing.T) {
	const id = "sb-ok"
	s, notifies := realServerWithAgent(t, id, true)

	w := forkRequest(t, s, id, id+"-tmpl")
	if w.Code != http.StatusOK {
		t.Fatalf("real-mode fork with reseeding guest: status %d, body %s", w.Code, w.Body.String())
	}
	if len(*notifies) != 1 {
		t.Fatalf("expected exactly one notify_forked, got %d", len(*notifies))
	}
	if len((*notifies)[0].Entropy) != 32 {
		t.Errorf("entropy length = %d, want 32", len((*notifies)[0].Entropy))
	}
}

// TestRealModeForkFailsClosedWhenGuestDoesNotReseed is the security regression
// guard: a real-mode fork whose guest reports ReseededRNG:false must FAIL
// CLOSED. The fork is rejected and the sandbox is not left registered, so an
// un-reseeded VM that shares CRNG state with its siblings is never served.
func TestRealModeForkFailsClosedWhenGuestDoesNotReseed(t *testing.T) {
	const id = "sb-noreseed"
	s, _ := realServerWithAgent(t, id, false)

	w := forkRequest(t, s, id, id+"-tmpl")
	if w.Code == http.StatusOK {
		t.Fatalf("real-mode fork must fail when the guest reports ReseededRNG:false; got 200 body %s", w.Body.String())
	}
	s.mu.RLock()
	_, registered := s.sandboxes[id]
	s.mu.RUnlock()
	if registered {
		t.Fatal("un-reseeded fork must not be left registered")
	}
}

// TestRealModeForkRetriesReseedOnTransientFailure proves the standalone server
// RETRIES the reseed handshake when the guest agent drops the first connection
// without replying. After a snapshot restore the guest resets its vsock listener,
// so the first NotifyForked can see "connection closed"; this is a readiness
// race, not a reseed refusal, and the fork must reconnect and retry rather than
// fail. The agent here drops the first two notify attempts, then reseeds; the
// fork must still succeed.
func TestRealModeForkRetriesReseedOnTransientFailure(t *testing.T) {
	const id = "sb-flaky"
	sock := fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentPort)
	_ = os.Remove(sock)
	seen := flakyAgent(t, sock, 2) // drop the first two reseed attempts

	dataDir, err := os.MkdirTemp("/tmp", "sbsrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dataDir) })
	s := newServer(dataDir, "", false, 16, 86400) // real mode
	s.reseedBackoff = time.Millisecond            // keep the test fast
	s.templates[id+"-tmpl"] = &templateInfo{ID: id + "-tmpl", Ready: true}

	w := forkRequest(t, s, id, id+"-tmpl")
	if w.Code != http.StatusOK {
		t.Fatalf("fork must retry past a transient reseed failure; got status %d body %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(seen) < 3 {
		t.Fatalf("expected at least 3 notify attempts (2 dropped + 1 success), got %d", atomic.LoadInt32(seen))
	}
	s.mu.RLock()
	_, registered := s.sandboxes[id]
	s.mu.RUnlock()
	if !registered {
		t.Fatal("a fork that reseeded after retry must be registered")
	}
}

// createTemplateRequest POSTs /v1/templates with an optional Idempotency-Key
// header and returns the recorder.
func createTemplateRequest(t *testing.T, s *server, id, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id})
	r := httptest.NewRequest(http.MethodPost, "/v1/templates", bytes.NewReader(body))
	if idempotencyKey != "" {
		r.Header.Set("Idempotency-Key", idempotencyKey)
	}
	w := httptest.NewRecorder()
	s.handleCreateTemplate(w, r)
	return w
}

// forkRequestWithKey POSTs /v1/fork with an optional Idempotency-Key header.
func forkRequestWithKey(t *testing.T, s *server, id, template, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id, "template": template})
	r := httptest.NewRequest(http.MethodPost, "/v1/fork", bytes.NewReader(body))
	if idempotencyKey != "" {
		r.Header.Set("Idempotency-Key", idempotencyKey)
	}
	w := httptest.NewRecorder()
	s.handleFork(w, r)
	return w
}

// TestCreateTemplateIdempotencyKeyReturnsSameResource proves that two
// /v1/templates POSTs carrying the SAME Idempotency-Key but DIFFERENT bodies
// return the SAME template (the first one): a retry never double-creates. A
// different key creates a distinct template (issue #22 idempotency keys).
func TestCreateTemplateIdempotencyKeyReturnsSameResource(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400)

	w1 := createTemplateRequest(t, s, "tmpl-a", "key-1")
	if w1.Code != http.StatusOK {
		t.Fatalf("first create: status %d body %s", w1.Code, w1.Body.String())
	}
	var first templateInfo
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	// Same key, different requested id: must return the FIRST template, and must
	// not create a second one.
	w2 := createTemplateRequest(t, s, "tmpl-b", "key-1")
	if w2.Code != http.StatusOK {
		t.Fatalf("repeat create: status %d body %s", w2.Code, w2.Body.String())
	}
	var second templateInfo
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("same Idempotency-Key must return the same template: got %q want %q", second.ID, first.ID)
	}
	s.mu.RLock()
	_, createdB := s.templates["tmpl-b"]
	count := len(s.templates)
	s.mu.RUnlock()
	if createdB {
		t.Error("repeat with the same key must not create the second requested id")
	}
	if count != 1 {
		t.Errorf("expected exactly one template after an idempotent repeat, got %d", count)
	}

	// A different key creates a distinct template.
	w3 := createTemplateRequest(t, s, "tmpl-c", "key-2")
	if w3.Code != http.StatusOK {
		t.Fatalf("new-key create: status %d body %s", w3.Code, w3.Body.String())
	}
	var third templateInfo
	if err := json.Unmarshal(w3.Body.Bytes(), &third); err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatalf("a different Idempotency-Key must create a new template; got %q", third.ID)
	}
}

// TestForkIdempotencyKeyReturnsSameSandbox proves that two /v1/fork POSTs with
// the same Idempotency-Key return the same sandbox in mock mode, and a different
// key forks a new one.
func TestForkIdempotencyKeyReturnsSameSandbox(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400)
	if w := createTemplateRequest(t, s, "python", ""); w.Code != http.StatusOK {
		t.Fatalf("create template: %s", w.Body.String())
	}

	w1 := forkRequestWithKey(t, s, "sb-1", "python", "fork-key-1")
	if w1.Code != http.StatusOK {
		t.Fatalf("first fork: status %d body %s", w1.Code, w1.Body.String())
	}
	var first sandboxInfo
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	// Same key, different requested id: must return the FIRST sandbox.
	w2 := forkRequestWithKey(t, s, "sb-2", "python", "fork-key-1")
	if w2.Code != http.StatusOK {
		t.Fatalf("repeat fork: status %d body %s", w2.Code, w2.Body.String())
	}
	var second sandboxInfo
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("same Idempotency-Key must return the same sandbox: got %q want %q", second.ID, first.ID)
	}
	s.mu.RLock()
	count := len(s.sandboxes)
	s.mu.RUnlock()
	if count != 1 {
		t.Errorf("expected exactly one sandbox after an idempotent repeat, got %d", count)
	}

	// A different key forks a new sandbox.
	w3 := forkRequestWithKey(t, s, "sb-3", "python", "fork-key-2")
	if w3.Code != http.StatusOK {
		t.Fatalf("new-key fork: status %d body %s", w3.Code, w3.Body.String())
	}
	var third sandboxInfo
	if err := json.Unmarshal(w3.Body.Bytes(), &third); err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatalf("a different Idempotency-Key must fork a new sandbox; got %q", third.ID)
	}
}

// TestResolveNetworkConfigSecureDefault asserts that a nil network config
// resolves to the secure default: deny-by-default egress AND inbound (issue
// #219). An untrusted sandbox with no policy can neither reach out nor be dialed.
func TestResolveNetworkConfigSecureDefault(t *testing.T) {
	got, err := resolveNetworkConfig(nil)
	if err != nil {
		t.Fatalf("resolveNetworkConfig(nil): %v", err)
	}
	if got.Egress != "deny" {
		t.Errorf("default egress = %q, want deny", got.Egress)
	}
	if got.Inbound != "deny" {
		t.Errorf("default inbound = %q, want deny-by-default", got.Inbound)
	}
}

// TestResolveNetworkConfigValidatesCIDRs asserts a malformed CIDR is rejected
// fail-closed rather than silently dropped, and that the knobs round-trip.
func TestResolveNetworkConfigValidatesCIDRs(t *testing.T) {
	if _, err := resolveNetworkConfig(&networkConfig{AllowCIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Error("expected error for malformed allow_cidrs")
	}
	if _, err := resolveNetworkConfig(&networkConfig{Inbound: "allow", InboundCIDRs: []string{"bad"}}); err == nil {
		t.Error("expected error for malformed inbound_cidrs")
	}
	got, err := resolveNetworkConfig(&networkConfig{
		Block:        true,
		Egress:       "deny",
		AllowCIDRs:   []string{"10.0.0.0/8"},
		Inbound:      "allow",
		InboundCIDRs: []string{"203.0.113.0/24"},
		AllowDomains: []string{"api.example.com:443"},
	})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if !got.Block || len(got.AllowCIDRs) != 1 || got.Inbound != "allow" {
		t.Errorf("config did not round-trip: %+v", got)
	}
}

// TestToNetworkPolicyMapsAllKnobs asserts the REST networkConfig maps onto the
// CRD NetworkPolicy that drives the datapath, so the standalone and k8s paths
// share one policy model (issue #219).
func TestToNetworkPolicyMapsAllKnobs(t *testing.T) {
	p := toNetworkPolicy(&networkConfig{
		Block:        true,
		Egress:       "allow",
		AllowDomains: []string{"api.example.com:443"},
		AllowCIDRs:   []string{"10.0.0.0/8"},
		Inbound:      "allow",
		InboundCIDRs: []string{"203.0.113.0/24"},
	})
	if !p.BlockNetwork {
		t.Error("block not mapped")
	}
	if string(p.Egress) != "allow" || len(p.Allow) != 1 || len(p.AllowCIDRs) != 1 {
		t.Errorf("egress/allow not mapped: %+v", p)
	}
	if string(p.Inbound) != "allow" || len(p.InboundCIDRs) != 1 {
		t.Errorf("inbound not mapped: %+v", p)
	}
	// A nil config maps to the fail-closed default deny policy.
	if d := toNetworkPolicy(nil); string(d.Egress) != "deny" {
		t.Errorf("nil config must map to deny, got %q", d.Egress)
	}
}

// TestIdempotencyReserveBlocksConcurrentCreate proves the check-and-reserve is
// atomic: a second caller arriving with the same key while the first create is
// still in flight is told in-flight, NOT allowed to proceed to a second create.
// The previous lookup-then-release-then-create left a window where two concurrent
// requests both missed and both created, defeating idempotency.
func TestIdempotencyReserveBlocksConcurrentCreate(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400)

	id, st := s.beginIdempotent("k1", idempotencyTemplate)
	if st != idemProceed || id != "" {
		t.Fatalf("first caller must proceed with no prior id, got st=%v id=%q", st, id)
	}
	if _, st2 := s.beginIdempotent("k1", idempotencyTemplate); st2 != idemInFlight {
		t.Fatalf("a concurrent same-key caller must see in-flight, not proceed to a second create, got %v", st2)
	}

	s.mu.Lock()
	s.recordIdempotent("k1", idempotencyTemplate, "tmpl-1")
	s.mu.Unlock()

	id3, st3 := s.beginIdempotent("k1", idempotencyTemplate)
	if st3 != idemReplay || id3 != "tmpl-1" {
		t.Fatalf("after completion a repeat must replay tmpl-1, got st=%v id=%q", st3, id3)
	}
}

// TestIdempotencyReleaseAllowsRetryAfterFailure proves a failed create does not
// consume the key: after releaseIdempotent a retry proceeds rather than being
// stuck reporting in-flight forever.
func TestIdempotencyReleaseAllowsRetryAfterFailure(t *testing.T) {
	s := newServer(t.TempDir(), "", true, 16, 86400)

	if _, st := s.beginIdempotent("k1", idempotencyFork); st != idemProceed {
		t.Fatalf("first caller must proceed")
	}
	s.releaseIdempotent("k1")
	if _, st := s.beginIdempotent("k1", idempotencyFork); st != idemProceed {
		t.Fatalf("after release a retry must proceed, not stay in-flight, got %v", st)
	}
}
