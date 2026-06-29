# Per-sandbox egress proxy live-fork (#336) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Unblock `Engine.ForkRunning` for networked sandboxes by routing guest HTTP/HTTPS egress through a per-node, host-owned egress proxy, so a live-forked child re-dials fresh upstreams instead of inheriting captured sockets.

**Architecture:** A per-node proxy inside forkd owns every upstream socket and attributes connections by source guest IP. A fixed sentinel `HTTP_PROXY` value baked into the template is DNATed per fork to the fork's own gateway, so the baked value is fork-stable while routing per fork. On a live fork the existing cold-fork NIC rebind + a fresh `/30` identity + an eth0 re-address + a conntrack flush + a `ResetUpstreams` signal force the child to re-dial.

**Tech Stack:** Go (forkd engine, daemon, netconf/nftables, conntrack), Rust (guest agent), protobuf (vsock control protocol), bash (KVM acceptance + bench).

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere (source, comments, docs, commit messages). ASCII hyphen only.
- Go: error wrapping `fmt.Errorf("context: %w", err)`; octal `0o644`; gofmt + golangci-lint clean is a merge gate. Run BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Secret values are never logged, never in errors, never in condition messages, never written to host paths. Log keys and counts only. The proxy logs upstream `host:port` and byte counts only, never path/query/headers/body/auth values.
- TDD: write the failing test first; every behavior change lands with its test in the same commit.
- DCO: every commit carries `Signed-off-by` (use `git commit -s`).
- Stage explicit paths only; never `git add -A`.
- Security-sensitive paths (`internal/fork`, `internal/daemon`, `guest/agent-rs`): a threat-model delta and a fork-correctness delta land in this same PR.
- Conventional commits: `feat`, `fix`, `docs`, `test`, `refactor`.
- Work happens in the worktree `.claude/worktrees/issue-336` on branch `feat/issue-336-egress-proxy-live-fork`.

## File structure

- `internal/netconf/nftables.go` (modify): add `RenderProxyDNAT`. Test: `internal/netconf/nftables_test.go`.
- `internal/egressproxy/proxy.go` (create): pure proxy logic (request parsing, CONNECT, attribution, redaction). Test: `internal/egressproxy/proxy_test.go`.
- `internal/egressproxy/listener_linux.go` (create): Linux listener bind + accept loop.
- `internal/network/conntrack.go` + `conntrack_linux.go` (create): `Flusher` interface + Linux exec impl + fake. Test: `internal/network/conntrack_test.go`.
- `proto/sandbox/controlv1/internal.proto` (modify): add `proxy_endpoint`, `reset_upstreams` to `NotifyForkedNetwork`.
- `internal/vsock/protocol.go` (modify): mirror the two fields on `NotifyForkedNetwork`.
- `internal/daemon/sandbox_api.go` (modify): map the two fields onto the proto request.
- `internal/daemon/server.go` (modify): `notifyForkedRunning` delivers the live fork's identity + proxy fields.
- `internal/fork/engine.go` (modify): proxy registration in `prepareForkNetwork`/`teardownForkNetwork`; unblock the `ForkRunning` gate; live-path conntrack flush. `EngineOpts` + engine fields for the proxy.
- `internal/fork/mock.go` (modify): `ForkRunning` returns a `GuestNetwork` so daemon tests exercise the new path.
- `cmd/forkd/main.go` (modify): `--egress-proxy` flag + sentinel/port flags; build + start the proxy; wire into `EngineOpts`.
- `guest/agent-rs/src/fork/network.rs` (modify): `NetworkConfig` gains `proxy_endpoint` + `reset_upstreams`; reset path drops stale route/neighbor + writes the proxy env file.
- `guest/agent-rs/src/service/` notify-forked handler (modify): plumb the two fields from the request.
- `docs/threat-model.md` (modify): egress-proxy row.
- `docs/fork-correctness.md` (modify): upstream-socket-inheritance row.
- `.github/workflows/kvm-test.yaml` (modify): `networked-live-fork` acceptance phase.
- `bench/networked-live-fork-latency.sh` (create): latency + N-way fan-out bench.

---

### Task 1: netconf sentinel proxy DNAT rendering

**Files:**
- Modify: `internal/netconf/nftables.go`
- Test: `internal/netconf/nftables_test.go`

**Interfaces:**
- Consumes: existing `SharedTableName()`, `SandboxChainName(tap)`, `NatTableName()`, `RenderMasquerade`.
- Produces: `RenderProxyDNAT(tap string, sentinel net.IP, proxyPort int, gatewayIP net.IP) string` returning nft commands that DNAT `tcp daddr <sentinel> dport <proxyPort>` to `<gatewayIP>:<proxyPort>` for traffic from this tap, in the nat table. The egress filter chain must also ACCEPT traffic to `<gatewayIP>:<proxyPort>` (the proxy listener) ahead of the allowlist; add `RenderProxyAccept(table, chain string, guestIP, gatewayIP net.IP, proxyPort int) string`.

- [ ] **Step 1: Write the failing test**

```go
func TestRenderProxyDNAT(t *testing.T) {
	out := RenderProxyDNAT("mitos0", net.ParseIP("169.254.169.2"), 3128, net.ParseIP("10.200.0.5"))
	// DNAT the fork-stable sentinel to THIS fork's gateway:proxyport.
	if !strings.Contains(out, "ip daddr 169.254.169.2 tcp dport 3128") {
		t.Fatalf("missing sentinel match: %q", out)
	}
	if !strings.Contains(out, "dnat to 10.200.0.5:3128") {
		t.Fatalf("missing dnat target: %q", out)
	}
	if !strings.Contains(out, NatTableName()) {
		t.Fatalf("dnat must live in the nat table: %q", out)
	}
}

func TestRenderProxyAccept(t *testing.T) {
	out := RenderProxyAccept(SharedTableName(), SandboxChainName("mitos0"),
		net.ParseIP("10.200.0.6"), net.ParseIP("10.200.0.5"), 3128)
	if !strings.Contains(out, "ip saddr 10.200.0.6 ip daddr 10.200.0.5 tcp dport 3128 accept") {
		t.Fatalf("proxy accept rule wrong: %q", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/netconf && go test -run 'TestRenderProxyDNAT|TestRenderProxyAccept' ./...`
Expected: FAIL (undefined `RenderProxyDNAT`, `RenderProxyAccept`).

- [ ] **Step 3: Implement**

Mirror the existing `RenderMasquerade` / `RenderMetadataBlock` style in `nftables.go`:

```go
// RenderProxyDNAT redirects the fork-stable sentinel proxy address to THIS
// fork's gateway, where the per-node egress proxy listens. The sentinel value
// is baked identically into every fork's HTTP_PROXY; the per-tap DNAT is what
// makes it route to this fork's own proxy context. All values are addresses,
// safe to log.
func RenderProxyDNAT(tap string, sentinel net.IP, proxyPort int, gatewayIP net.IP) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s tcp dport %d dnat to %s:%d\n",
		NatTableName(), proxyDNATChainName(), tap, sentinel, proxyPort, gatewayIP, proxyPort)
	return b.String()
}

// RenderProxyAccept allows the guest to reach the per-node proxy listener on the
// gateway address, ahead of the allowlist (the proxy, not the guest, enforces
// upstream egress policy via #494 later).
func RenderProxyAccept(table, chain string, guestIP, gatewayIP net.IP, proxyPort int) string {
	return fmt.Sprintf("add rule inet %s %s ip saddr %s ip daddr %s tcp dport %d accept\n",
		table, chain, guestIP, gatewayIP, proxyPort)
}
```

Add a `proxyDNATChainName()` helper next to the other `*Name()` funcs (a prerouting chain in the nat table; follow the existing nat-table chain creation pattern used for masquerade).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/netconf && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/netconf/nftables.go internal/netconf/nftables_test.go
git commit -s -m "feat(netconf): render sentinel proxy DNAT + accept rules (#336)"
```

---

### Task 2: egressproxy pure package (parsing, attribution, redaction)

**Files:**
- Create: `internal/egressproxy/proxy.go`
- Test: `internal/egressproxy/proxy_test.go`

**Interfaces:**
- Produces:
  - `type SandboxResolver interface { Lookup(srcIP net.IP) (sandboxID string, ok bool) }`
  - `type Dialer interface { Dial(ctx context.Context, hostport string) (net.Conn, error) }`
  - `type Logger interface { Egress(sandboxID, hostport string, bytesUp, bytesDown int64) }`
  - `type Proxy struct { ... }` with `NewProxy(r SandboxResolver, d Dialer, l Logger) *Proxy` and `Serve(client net.Conn, srcIP net.IP)`.
  - `func parseRequestTarget(line string) (method, hostport string, isConnect bool, err error)` (unexported, tested via exported behavior).
- Consumes: nothing from other tasks.

- [ ] **Step 1: Write the failing test**

```go
func TestServeConnectTunnelOpensUpstreamAndRedacts(t *testing.T) {
	// upstream stub echoes
	up := newEchoConn()
	d := &fakeDialer{conns: map[string]net.Conn{"api.example.com:443": up}}
	rec := &recordLogger{}
	p := NewProxy(staticResolver{ip2id: map[string]string{"10.0.0.6": "sbx-1"}}, d, rec)

	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.6"))
	// minimal CONNECT
	fmt.Fprintf(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nAuthorization: Bearer SECRET\r\n\r\n")
	br := bufio.NewReader(client)
	status, _ := br.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("want 200 tunnel established, got %q", status)
	}
	// the dialer was asked for the upstream host:port (host-owned upstream)
	if !d.dialed("api.example.com:443") {
		t.Fatal("upstream was not dialed host-side")
	}
	// redaction: no secret, no header name, no path reached the log
	for _, e := range rec.entries {
		if strings.Contains(e, "SECRET") || strings.Contains(e, "Authorization") {
			t.Fatalf("log leaked secret/header: %q", e)
		}
	}
	if rec.entries[0] != "sbx-1 api.example.com:443" {
		t.Fatalf("egress log should be sandbox + hostport only, got %q", rec.entries[0])
	}
}

func TestServeRejectsUnknownSource(t *testing.T) {
	p := NewProxy(staticResolver{}, &fakeDialer{}, &recordLogger{})
	client, server := net.Pipe()
	go p.Serve(server, net.ParseIP("10.0.0.99"))
	fmt.Fprintf(client, "CONNECT x:443 HTTP/1.1\r\n\r\n")
	st, _ := bufio.NewReader(client).ReadString('\n')
	if !strings.Contains(st, "403") {
		t.Fatalf("unknown source must be refused, got %q", st)
	}
}
```

(Define `fakeDialer`, `recordLogger`, `staticResolver`, `newEchoConn` as small test doubles in the test file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/egressproxy && go test ./...`
Expected: FAIL (package/symbols undefined).

- [ ] **Step 3: Implement `proxy.go`**

Implement: read the first request line + headers from the client; `parseRequestTarget` extracts `host:port` from a `CONNECT` line (`hostport` = the authority) or from the absolute-form request URI of a plain HTTP forward request, taking only the host and port (never the path or query). For `CONNECT`: resolve `srcIP` via `SandboxResolver` (403 + close on miss), `Dial` the upstream host-side, write `HTTP/1.1 200 Connection Established\r\n\r\n`, then bidirectionally copy, counting bytes; call `Logger.Egress(sandboxID, hostport, up, down)` once on close. For plain HTTP: dial host-side, replay the request, stream the response, counting bytes; same single redacted log line. Never buffer or log bodies/headers. The `Logger` receives only `sandboxID` and `hostport` (the test pins the exact format `"<id> <hostport>"`).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/egressproxy && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/egressproxy/proxy.go internal/egressproxy/proxy_test.go
git commit -s -m "feat(egressproxy): host-owned CONNECT/HTTP proxy with source-IP attribution and redaction (#336)"
```

---

### Task 3: egressproxy Linux listener + engine registration + forkd flag

**Files:**
- Create: `internal/egressproxy/listener_linux.go`
- Modify: `internal/fork/engine.go` (EngineOpts + fields + register/deregister), `cmd/forkd/main.go`
- Test: extend `internal/fork/engine_test.go` (registration), `internal/egressproxy/proxy_test.go` (resolver registry)

**Interfaces:**
- Consumes: `Proxy` (Task 2), `RenderProxyDNAT`/`RenderProxyAccept` (Task 1).
- Produces:
  - `type Registry struct{}` in `internal/egressproxy` with `NewRegistry() *Registry`, `Register(guestIP net.IP, sandboxID string)`, `Deregister(guestIP net.IP)`, and it implements `SandboxResolver`. (Mirror `dnsproxy.Registry`.)
  - On the engine: `EngineOpts.EgressProxy *egressproxy.Registry`, `EngineOpts.ProxySentinel net.IP`, `EngineOpts.ProxyPort int`; engine fields `egressProxy`, `proxySentinel`, `proxyPort`; helper `egressProxyEnabled() bool` (registry non-nil AND networking on).

- [ ] **Step 1: Write the failing test** (engine registration)

```go
func TestPrepareForkNetworkRegistersEgressProxy(t *testing.T) {
	reg := egressproxy.NewRegistry()
	e := newTestEngineWithNetwork(t, withEgressProxy(reg, net.ParseIP("169.254.169.2"), 3128))
	fn, err := e.prepareForkNetwork("sbx-1", ForkOpts{Network: &NetworkOpts{}})
	if err != nil { t.Fatal(err) }
	if id, ok := reg.Lookup(fn.identity.GuestIP); !ok || id != "sbx-1" {
		t.Fatalf("guest IP not registered with proxy: %v %v", id, ok)
	}
	// guestNet carries the fork-stable sentinel endpoint
	if fn.guestNet.ProxyEndpoint != "169.254.169.2:3128" {
		t.Fatalf("proxy endpoint not delivered: %q", fn.guestNet.ProxyEndpoint)
	}
	e.teardownForkNetwork("sbx-1", fn.identity)
	if _, ok := reg.Lookup(fn.identity.GuestIP); ok {
		t.Fatal("guest IP still registered after teardown")
	}
}
```

(`withEgressProxy` is a test option helper; `ProxyEndpoint` is added to `vsock.NotifyForkedNetwork` in Task 4. To keep Task 3 self-contained, add the `ProxyEndpoint` field to the struct as part of this task's first commit, then Task 4 wires the proto.)

- [ ] **Step 2: Run test to verify it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env); go test ./internal/fork/ -run TestPrepareForkNetworkRegistersEgressProxy`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `engine.go` `prepareForkNetwork`, after `netMgr.Setup`, when `egressProxyEnabled()`: call `e.egressProxy.Register(id.GuestIP, sandboxID)`, set `guestNet.ProxyEndpoint = net.JoinHostPort(e.proxySentinel.String(), strconv.Itoa(e.proxyPort))`, and have `netMgr.Setup` emit the proxy DNAT + accept rules (extend `SandboxPolicy` with `ProxySentinel net.IP` + `ProxyPort int`, rendered in `internal/network/network_apply.go` using Task 1's helpers). In `teardownForkNetwork`, `Deregister(id.GuestIP)`. Add the `EngineOpts` fields + engine fields + `egressProxyEnabled()`. Create `listener_linux.go`: `func (p *Proxy) ListenAndServe(addr string) error` accept loop that derives `srcIP` from `conn.RemoteAddr()` and calls `p.Serve`. In `cmd/forkd/main.go`, mirror the DNS-proxy wiring: `--egress-proxy` bool, `--proxy-sentinel` (default `169.254.169.2`), `--proxy-port` (default `3128`); build `egressproxy.NewRegistry()`, set `engineOpts.EgressProxy/ProxySentinel/ProxyPort`, and start the listener as a Runnable on `<gatewayBindAddr>:<proxyPort>` (bind on the resolver-style node address; the DNAT targets each fork's gateway, all on the same forkd process).

- [ ] **Step 4: Run tests to verify pass**

Run: `go test ./internal/fork/ -run TestPrepareForkNetwork; go test ./internal/egressproxy/...; go build ./cmd/forkd`
Expected: PASS / build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/egressproxy/ internal/fork/engine.go internal/network/ cmd/forkd/main.go internal/fork/engine_test.go
git commit -s -m "feat(forkd): wire per-node egress proxy registry + sentinel DNAT into fork networking (#336)"
```

---

### Task 4: proto + vsock + daemon mapping for proxy endpoint and reset

**Files:**
- Modify: `proto/sandbox/controlv1/internal.proto`, `internal/vsock/protocol.go`, `internal/daemon/sandbox_api.go`
- Test: `internal/vsock/protocol_test.go` (or a daemon mapping test)

**Interfaces:**
- Produces: `NotifyForkedNetwork.ProxyEndpoint string` and `.ResetUpstreams bool` on both the Go vsock struct and the proto message (fields 6 and 7), mapped in `sandbox_api.go NotifyForked`.

- [ ] **Step 1: Write the failing test**

```go
func TestNotifyForkedNetworkProxyFieldsMap(t *testing.T) {
	gn := &vsock.NotifyForkedNetwork{GuestIP: "10.0.0.6", GatewayIP: "10.0.0.5", PrefixLen: 30,
		ProxyEndpoint: "169.254.169.2:3128", ResetUpstreams: true}
	req := buildNotifyForkedRequest(7, nil, gn, nil) // helper that mirrors sandbox_api mapping
	if req.Network.ProxyEndpoint != "169.254.169.2:3128" || !req.Network.ResetUpstreams {
		t.Fatalf("proxy fields not mapped: %+v", req.Network)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestNotifyForkedNetworkProxyFields`
Expected: FAIL (fields undefined).

- [ ] **Step 3: Implement**

Add to `internal.proto` `NotifyForkedNetwork`:

```proto
  // proxy_endpoint is the fork-stable HTTP(S) proxy address (host:port) the
  // guest egresses through. Config, safe to log.
  string proxy_endpoint = 6;
  // reset_upstreams tells the guest this is a live fork: drop stale route and
  // neighbor state after re-addressing eth0 so captured upstream sockets die
  // and clients re-dial through the proxy. Safe to log.
  bool reset_upstreams = 7;
```

Run `make proto`. Add the two fields to `vsock.NotifyForkedNetwork` (with the doc-comment punctuation rules) and map them in `sandbox_api.go NotifyForked` alongside `GuestIp`/`GatewayIp`. Add the `buildNotifyForkedRequest` test helper if one does not exist (extract the existing mapping in `sandbox_api.go` into a small pure function so it is testable).

- [ ] **Step 4: Run test to verify it passes**

Run: `make proto && go test ./internal/daemon/ ./internal/vsock/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add proto/sandbox/controlv1/internal.proto internal/vsock/protocol.go internal/daemon/sandbox_api.go internal/daemon/*_test.go
git commit -s -m "feat(proto): carry proxy_endpoint and reset_upstreams in NotifyForked (#336)"
```

---

### Task 5: Rust guest agent reset-upstreams path

**Files:**
- Modify: `guest/agent-rs/src/fork/network.rs`, the notify-forked handler in `guest/agent-rs/src/service/`
- Test: `guest/agent-rs/src/fork/network.rs` `#[cfg(test)]`

**Interfaces:**
- Consumes: the two new proto fields (Task 4) decoded into the request.
- Produces: `NetworkConfig { ..., proxy_endpoint: String, reset_upstreams: bool }`; `configure_network_on` writes `proxy_endpoint` to a known env file (`/etc/profile.d/mitos-proxy.sh` exporting `HTTP_PROXY`/`HTTPS_PROXY`) and, when `reset_upstreams`, flushes stale neighbor/route state after re-addressing.

- [ ] **Step 1: Write the failing test**

```rust
#[test]
fn writes_proxy_env_file_on_reset() {
    let dir = tempdir().unwrap();
    let env_path = dir.path().join("mitos-proxy.sh");
    let cfg = NetworkConfig {
        guest_ip: "10.0.0.6".into(), gateway_ip: "10.0.0.5".into(), prefix_len: 30,
        guest_mac: String::new(), resolver_ip: String::new(),
        proxy_endpoint: "169.254.169.2:3128".into(), reset_upstreams: true,
    };
    write_proxy_env(&env_path, &cfg.proxy_endpoint).unwrap();
    let body = std::fs::read_to_string(&env_path).unwrap();
    assert!(body.contains("HTTP_PROXY=http://169.254.169.2:3128"));
    assert!(body.contains("HTTPS_PROXY=http://169.254.169.2:3128"));
    // no secrets, just the endpoint
    assert!(!body.contains("Authorization"));
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd guest/agent-rs && cargo test writes_proxy_env_file_on_reset`
Expected: FAIL (field/function missing). Verify via `rust:1-bookworm` docker if no local Rust: `docker run --rm -v "$PWD":/w -w /w/guest/agent-rs rust:1-bookworm cargo test`.

- [ ] **Step 3: Implement**

Add the two fields to `NetworkConfig`. Add `fn write_proxy_env(path: &Path, endpoint: &str) -> io::Result<()>` writing the export lines (empty endpoint writes nothing / removes the file). In `apply_linux`, after a successful re-address, call `write_proxy_env`; when `reset_upstreams`, call a new `crate::sys::netlink::flush_neighbors(iface)` (best-effort, log + continue on error, mirroring the existing fail-closed-but-continue style). Plumb `proxy_endpoint`/`reset_upstreams` from the decoded request in the notify-forked handler.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd guest/agent-rs && cargo test` (or the docker invocation above).
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add guest/agent-rs/src/fork/network.rs guest/agent-rs/src/service/
git commit -s -m "feat(guest): write proxy env and reset stale neighbors on live-fork (#336)"
```

---

### Task 6: host conntrack flush seam

**Files:**
- Create: `internal/network/conntrack.go`, `internal/network/conntrack_linux.go`
- Test: `internal/network/conntrack_test.go`

**Interfaces:**
- Produces: `type Flusher interface { FlushSource(ctx context.Context, guestIP net.IP) error }`, a `FakeFlusher` recording calls, and a Linux impl `(m *linuxManager) FlushSource` that runs `conntrack -D -s <guestIP>` via the existing command runner seam (`m.run`). Add `FlushSource` to the `Manager` interface.

- [ ] **Step 1: Write the failing test**

```go
func TestFlushSourceBuildsConntrackDeleteByGuestIP(t *testing.T) {
	rec := &recordRunner{}
	m := newLinuxManagerForTest(rec)
	if err := m.FlushSource(context.Background(), net.ParseIP("10.200.0.6")); err != nil {
		t.Fatal(err)
	}
	if !rec.ran("conntrack", "-D", "-s", "10.200.0.6") {
		t.Fatalf("conntrack delete not issued: %v", rec.calls)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/network/ -run TestFlushSource`
Expected: FAIL.

- [ ] **Step 3: Implement** the `Flusher` interface, `FakeManager.FlushSource` (record), and the linux impl using `m.run`. Treat "no entries deleted" exit status as success (conntrack returns nonzero when nothing matched; match on that and return nil).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/network/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/network/conntrack.go internal/network/conntrack_linux.go internal/network/conntrack_test.go internal/network/network.go
git commit -s -m "feat(network): conntrack flush-by-source seam for live-fork upstream reset (#336)"
```

---

### Task 7: unblock ForkRunning + deliver live identity + conntrack flush

**Files:**
- Modify: `internal/fork/engine.go` (gate + live-path network), `internal/daemon/server.go` (`notifyForkedRunning`), `internal/fork/mock.go`
- Test: `internal/fork/engine_test.go`, `internal/daemon/server_test.go`

**Interfaces:**
- Consumes: `egressProxyEnabled()` (Task 3), `Flusher` (Task 6), proxy fields (Task 4).
- Produces: `ForkResult.GuestNetwork` populated on the live path; gate removed; #18 reference gone.

- [ ] **Step 1: Write the failing tests**

```go
// engine: live fork of a networked sandbox no longer fails closed when the proxy is active
func TestForkRunningNetworkedSucceedsWithProxy(t *testing.T) {
	reg := egressproxy.NewRegistry()
	e := newTestEngineWithNetwork(t, withEgressProxy(reg, net.ParseIP("169.254.169.2"), 3128))
	src := mustFork(t, e, "src")
	res, err := e.ForkRunning(src.SandboxID, "child", true)
	if err != nil { t.Fatalf("live fork must succeed with proxy active: %v", err) }
	if res.GuestNetwork == nil || res.GuestNetwork.GuestIP == src.GuestNetwork.GuestIP {
		t.Fatal("child must get a fresh per-fork identity")
	}
	if !res.GuestNetwork.ResetUpstreams { t.Fatal("live fork must set ResetUpstreams") }
}

func TestForkRunningNetworkedFailsClosedWithoutProxy(t *testing.T) {
	e := newTestEngineWithNetwork(t) // no proxy
	src := mustFork(t, e, "src")
	_, err := e.ForkRunning(src.SandboxID, "child", true)
	if err == nil || !strings.Contains(err.Error(), "#336") {
		t.Fatalf("must fail closed referencing #336, got %v", err)
	}
	if strings.Contains(err.Error(), "#18") { t.Fatal("stale #18 reference") }
}
```

```go
// daemon: notifyForkedRunning now delivers the live fork's identity + proxy fields
func TestNotifyForkedRunningDeliversIdentity(t *testing.T) {
	// arrange a mock engine whose ForkRunning returns a GuestNetwork; assert the
	// fake sandboxAPI received a NotifyForked carrying that identity + ResetUpstreams.
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/fork/ -run TestForkRunning; go test ./internal/daemon/ -run TestNotifyForkedRunning`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `engine.go ForkRunning`: replace the fail-closed block with: if `e.networkEnabled() && !e.egressProxyEnabled()` return `fmt.Errorf("live fork of a networked sandbox requires the egress proxy; start forkd with --egress-proxy (tracked in #336)")`. Otherwise proceed; pass a non-zero `ForkOpts{Network: ...}` derived from the source so `fork` runs `prepareForkNetwork` (fresh identity + overrides + proxy registration). After the child resumes, call `e.netMgr.FlushSource(ctx, childGuestIP)` (best-effort). Ensure `fork` returns `GuestNetwork` with `ResetUpstreams=true` on the live path (thread a flag into `fork`/`forkNetwork`). In `server.go notifyForkedRunning`, stop passing `nil`: pass `result.GuestNetwork` (now populated) so the guest re-addresses + resets. In `mock.go ForkRunning`, populate `GuestNetwork` (fresh guest IP via the counter) so daemon tests exercise the path.

- [ ] **Step 4: Run tests to verify pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env); go test ./internal/fork/ ./internal/daemon/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/fork/engine.go internal/fork/mock.go internal/daemon/server.go internal/fork/engine_test.go internal/daemon/server_test.go
git commit -s -m "feat(fork): unblock live-fork of networked sandboxes via egress proxy (#336)"
```

---

### Task 8: threat-model + fork-correctness rows

**Files:**
- Modify: `docs/threat-model.md`, `docs/fork-correctness.md`

- [ ] **Step 1: Add the fork-correctness row.** Follow the existing table/row format in `docs/fork-correctness.md`. Hazard: a fork inheriting a live upstream socket (shared 4-tuple/seq/TLS across parent and child). Invariant: a fork must not inherit a live upstream socket; the child re-dials through the per-sandbox egress proxy. Mechanism: host-owned upstream sockets, per-fork eth0 re-address, fork-boundary conntrack flush, `ResetUpstreams`. Status + the verifying test name (`networked-live-fork` KVM phase, Task 9).

- [ ] **Step 2: Add the threat-model row.** Follow the per-row status format in `docs/threat-model.md`. Surface: the egress proxy on secret-bearing traffic. Mitigation: CONNECT pass-through (no TLS interception), logs `host:port` + byte counts only, per-sandbox attribution by source IP, composes with the nft allowlist + unconditional metadata drops. Residual: a plain-HTTP request's target path is visible to the proxy but never logged; agents steered to HTTPS.

- [ ] **Step 3: Verify no banned dashes.**

Run: `grep -nP '[\x{2014}\x{2013}]' docs/threat-model.md docs/fork-correctness.md && echo FOUND || echo clean`
Expected: `clean`.

- [ ] **Step 4: Commit**

```bash
git add docs/threat-model.md docs/fork-correctness.md
git commit -s -m "docs: threat-model + fork-correctness rows for egress proxy live-fork (#336)"
```

---

### Task 9: KVM acceptance phase

**Files:**
- Modify: `.github/workflows/kvm-test.yaml`
- Create: the harness the phase invokes (a small Go test under a `kvm` build tag or a script under `bench/`/`hack/`, following the existing `husk-stub`/`husk-probe` phase pattern).

- [ ] **Step 1: Study the existing `husk-stub` / `husk-activate-correctness` phases** in `kvm-test.yaml` to mirror structure, gating, and the honest target-vs-measured framing.

- [ ] **Step 2: Implement the `networked-live-fork` phase.** Boot a networked sandbox; start a local upstream stub (a tiny HTTP server on the host the guest reaches through the proxy) that records distinct client connections; have the guest open a keep-alive and issue a request; live-fork; in parent and child issue a request each. Gate (hard-fail) on: (a) both parent and child get a 200 through independent egress, (b) distinct tap/MAC/guest-IP and no socket 4-tuple collision, (c) the stub observes a NEW upstream connection for the child (captured socket not reused), (d) the fork-correctness handshake reports `ReseededRNG` + a clock step. Mark transient nested-KVM boot steps with the retry the other husk phases use; the assertions (a)-(d) hard-fail.

- [ ] **Step 3: Lint the workflow** for banned dashes and YAML validity (`grep -nP '[\x{2014}\x{2013}]' .github/workflows/kvm-test.yaml`).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/kvm-test.yaml <harness files>
git commit -s -m "test(kvm): networked live-fork acceptance for egress proxy (#336)"
```

---

### Task 10: bench (no-unverified-claims)

**Files:**
- Create: `bench/networked-live-fork-latency.sh`
- Modify: `bench/README.md` (document the new measurement)

- [ ] **Step 1: Study `bench/husk-activate-latency.sh` and `bench/workspace-fork-latency.sh`** for the measurement + results format.

- [ ] **Step 2: Implement** a script that measures networked live-fork latency (live-fork-to-first-egress) vs the cold-fork networked path, plus an N-way fan-out (fork N children from one networked source, record per-fork latency distribution). Emit results into `bench/results/` in the existing format. Numbers must be reproducible from the script alone.

- [ ] **Step 3: Document** the measurement + how to run it in `bench/README.md`.

- [ ] **Step 4: Commit**

```bash
git add bench/networked-live-fork-latency.sh bench/README.md
git commit -s -m "bench: networked live-fork latency vs cold-fork + N-way fan-out (#336)"
```

---

### Task 11: full local verification + ref-hygiene sweep + PR

- [ ] **Step 1: Run the full local suite.**

```bash
gofmt -l . | (grep . && echo UNFORMATTED || echo gofmt-clean)
go build ./... && go vet ./...
make test-unit
eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ ./internal/fork/ ./internal/daemon/ ./internal/network/ ./internal/netconf/ ./internal/egressproxy/
make test-python   # unaffected, sanity only
golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m
```
Expected: all green. Fix anything red before proceeding (systematic-debugging if needed).

- [ ] **Step 2: Confirm no stale `#18` reference remains** for the live-fork gate and no banned dashes anywhere in the diff:

```bash
git diff origin/main --name-only | xargs grep -nP '[\x{2014}\x{2013}]' && echo FOUND || echo clean
grep -rn "not supported yet; tracked in #18" internal/ || echo "gate ref updated"
```

- [ ] **Step 3: Push and open the PR.**

```bash
git push -u origin feat/issue-336-egress-proxy-live-fork
gh pr create --title "feat(fork): live-fork networked sandboxes via per-sandbox egress proxy (#336)" \
  --body "<summary, design link, threat-model + fork-correctness deltas, test matrix incl. KVM acceptance, bench; Closes #336; security-sensitive path: requesting a named human reviewer>"
```

Expected: CI runs the eight required checks plus the `kvm-test.yaml` `networked-live-fork` phase on the self-hosted runners. The PR stays open for the named human review CLAUDE.md requires on `internal/fork`/`internal/daemon`/`guest/agent-rs`.

---

## Self-review notes

- Spec coverage: proxy model (T2/T3), sentinel + DNAT (T1/T3), gate unblock + live identity (T7), NotifyForked extension (T4/T5), conntrack flush (T6), threat-model + fork-correctness (T8), KVM acceptance (T9), bench (T10), full verify + ref-hygiene (T11). All spec sections map to a task.
- Type consistency: `NotifyForkedNetwork.ProxyEndpoint`/`ResetUpstreams` used identically across Go vsock (T4), proto (T4), engine `guestNet` (T3/T7), and Rust `NetworkConfig` (T5). `egressproxy.Registry` implements `SandboxResolver` (T2/T3). `Manager.FlushSource` defined T6, consumed T7.
- Ordering: T1-T6 are independent-ish building blocks; T7 depends on T3/T4/T6; T8-T10 depend on the behavior existing; T11 is the gate. The `ProxyEndpoint` vsock field is introduced in T3 (struct) and wired to proto in T4 to keep T3's test self-contained.
