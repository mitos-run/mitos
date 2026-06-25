# Mitos Expose Slice 2a: edge proxy single-label subdomains and forkd backend

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Evolve the per-sandbox preview proxy into the Mitos Expose edge proxy: resolve a single-label subdomain `<label>.<expose-domain>` to a route, and reverse-proxy to the owning forkd node's slice-1 expose handler `http://<node>:9091/v1/sandboxes/{id}/expose/{port}/` with the per-sandbox bearer, behind a signed-link gate, with reserved-name and Host-allowlist defense and an authenticated admin route-sync endpoint.

**Architecture:** The proxy stays a standalone Go binary (`cmd/preview-proxy`) over `internal/preview`. This slice changes three things: the host scheme (drop the `.preview.` infix; the leftmost label is an opaque routing key), the route backend (a route now carries the owning node endpoint, the sandbox id, and the guest port, so the proxy builds the forkd expose URL instead of dialing a sandbox IP directly), and a new authenticated `POST /internal/routes` admin endpoint that calls `RouteTable.Sync` (this is how the slice-2b controller reconciler will feed routes). No Kubernetes client is added here; the route table is fed by the admin endpoint or tests. The signed-link verification from issue #126 is retained as the slice-2a access gate; the full sharing ladder (private/org/audience) is slice 4.

**Tech Stack:** Go standard library (`net/http`, `net/http/httputil`, `crypto/subtle`, `crypto/hmac`), the existing `internal/preview` package, the slice-1 forkd expose handler.

## Global Constraints

- Go module `mitos.run/mitos`; Go 1.24 or newer.
- Never use em (U+2014) or en (U+2013) dashes anywhere, including comments, docs, and commit messages; only `.` `,` `;` `:` and the ASCII hyphen-minus.
- Error wrapping `fmt.Errorf("context: %w", err)`; octal `0o644`.
- Bearer tokens, the signing secret, and the admin token are bearer credentials: never logged, never in an error body, condition, or host path. Log keys, counts, sandbox ids, labels, and HTTP status only.
- The proxy reaches forkd over plain HTTP on `:9091` (forkd serves the sandbox API in cleartext with bearer auth; this matches the existing SDK path and is not a new weakening). The bearer-over-cluster-network hop is a recorded threat-model item.
- The proxy never derives the upstream host from request input: the upstream node endpoint, sandbox id, and port come only from the route looked up by the validated label. The guest dial stays `127.0.0.1` inside forkd (slice 1, unchanged).
- TDD: failing test in the same commit as the behavior change. DCO sign-off (`git commit -s`) on every commit. Stage explicit paths only.
- Conventional commits: `feat`, `fix`, `docs`, `test`, `refactor`.
- Lint clean both `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m` for the touched packages.
- Deviation from the design spec, intentional and recorded here: the `internal/preview` -> `internal/expose` package and `cmd/preview-proxy` -> `cmd/expose-proxy` binary renames are DEFERRED (high churn, no functional value); this slice evolves `internal/preview` in place. The label is an opaque routing key; the `<port>-<name>` and stable-alias encoding is the controller's concern in slice 2b, not the proxy's.

---

### Task 1: single-label host parsing with configurable domain and reserved names

Replace the `<sandbox-id>.preview.<domain>` parse with `<label>.<expose-domain>`: the leftmost label is returned as an opaque routing key. A configurable `expose-domain` and a reserved-name blocklist are added. The route table (Task 2) is the Host allowlist: an unknown label is a 404.

**Files:**
- Modify: `internal/preview/route.go` (the `ParseHost` function and `previewLabel` constant)
- Modify: `internal/preview/url.go` (`MintURL` host construction, to match the new scheme)
- Test: `internal/preview/route_test.go` (extend the existing `ParseHost` tests), `internal/preview/url_test.go`

**Interfaces:**
- Produces: `func ParseHost(host, domain string) (label string, ok bool)` keeps its signature but now matches `<label>.<domain>` (no `preview` infix). `label` is a single DNS label.
- Produces: `func IsReservedLabel(label string) bool` over a fixed reserved set.

- [ ] **Step 1: Write the failing tests**

```go
// internal/preview/route_test.go (add; adapt existing ParseHost tests to the new scheme)
func TestParseHostSingleLabel(t *testing.T) {
	cases := []struct {
		host, domain, want string
		ok                 bool
	}{
		{"openclaw.mitos.run", "mitos.run", "openclaw", true},
		{"8000-sbx1.mitos.run", "mitos.run", "8000-sbx1", true},
		{"OpenClaw.Mitos.Run", "mitos.run", "openclaw", true}, // case-insensitive
		{"openclaw.mitos.run:443", "mitos.run", "openclaw", true}, // strips port
		{"a.b.mitos.run", "mitos.run", "", false},                 // two labels: reject
		{"mitos.run", "mitos.run", "", false},                     // apex: no label
		{"openclaw.evil.com", "mitos.run", "", false},             // wrong domain
		{"", "mitos.run", "", false},
		{"openclaw.sandbox.example.com", "sandbox.example.com", "openclaw", true}, // configurable domain
	}
	for _, c := range cases {
		got, ok := ParseHost(c.host, c.domain)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseHost(%q,%q)=(%q,%v) want (%q,%v)", c.host, c.domain, got, ok, c.want, c.ok)
		}
	}
}

func TestIsReservedLabel(t *testing.T) {
	for _, r := range []string{"www", "app", "api", "console", "admin", "auth", "login"} {
		if !IsReservedLabel(r) {
			t.Errorf("expected %q reserved", r)
		}
	}
	for _, ok := range []string{"openclaw", "8000-sbx1", "myapp"} {
		if IsReservedLabel(ok) {
			t.Errorf("did not expect %q reserved", ok)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/preview/ -run 'TestParseHostSingleLabel|TestIsReservedLabel' -v`
Expected: FAIL (old `ParseHost` requires the `.preview.` infix; `IsReservedLabel` undefined).

- [ ] **Step 3: Implement**

In `internal/preview/route.go`, replace the `previewLabel` based parse. Remove the `previewLabel` constant and rewrite `ParseHost`:

```go
// ParseHost extracts the single leftmost label from an expose hostname of the
// form <label>.<domain>. It strips any ":port", lowercases (DNS is case
// insensitive), and requires exactly one label to the left of the base domain
// (no embedded dots), so a multi-label host is rejected. The label is an opaque
// routing key; the caller resolves it against the route table. ok is false for
// any host that does not match, so the proxy can reject unknown vhosts.
func ParseHost(host, domain string) (label string, ok bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" || domain == "" {
		return "", false
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label = host[:len(host)-len(suffix)]
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// reservedLabels are hostnames a tenant may never take: control-plane and
// well-known names that would enable phishing or interception if served as an
// untrusted app. The set is the proxy's defensive backstop; the controller also
// rejects them at registration time (slice 2b).
var reservedLabels = map[string]struct{}{
	"www": {}, "app": {}, "api": {}, "console": {}, "gateway": {},
	"admin": {}, "auth": {}, "login": {}, "account": {}, "mail": {},
	"static": {}, "assets": {}, "cdn": {}, "status": {},
}

// IsReservedLabel reports whether label is reserved and must not route to a
// tenant app.
func IsReservedLabel(label string) bool {
	_, ok := reservedLabels[strings.ToLower(label)]
	return ok
}
```

In `internal/preview/url.go`, update `MintURL`'s host from `sandboxID + "." + previewLabel + "." + domain` to `sandboxID + "." + domain`. Adjust the doc comment to the new scheme. Update `url_test.go` expectations accordingly.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/preview/ -run 'TestParseHost|TestIsReserved|TestMintURL' -v`
Expected: PASS. Fix any other `route_test.go`/`url_test.go` cases that assumed the old `.preview.` scheme.

- [ ] **Step 5: Commit**

```bash
git add internal/preview/route.go internal/preview/url.go internal/preview/route_test.go internal/preview/url_test.go
git commit -s -m "feat(preview): single-label expose host scheme and reserved-name blocklist"
```

---

### Task 2: route carries the forkd backend (node endpoint, sandbox id, port)

A route now points at the owning forkd node's `:9091` plus the sandbox id and guest port, so the proxy can build the slice-1 expose URL. The map key becomes the opaque label (not the sandbox id), so multiple labels can target the same sandbox on different ports.

**Files:**
- Modify: `internal/preview/route.go` (the `Route`, `ClaimState` structs, `Upsert`/`Lookup`/`Remove`/`Sync` keyed by label)
- Test: `internal/preview/route_test.go`

**Interfaces:**
- Produces: `Route{ Label, SandboxID, NodeEndpoint, Port int, Token, Sharing string }`. `NodeEndpoint` is `host:port` of forkd `:9091` (from `Sandbox.Status.Endpoint`). `Token` is the per-sandbox bearer.
- Produces: `ClaimState{ Label, SandboxID, NodeEndpoint string, Port int, Token, Sharing string, Ready bool }`.
- Produces: `RouteTable.Lookup(label string) (Route, bool)`, `Upsert(Route)`, `Remove(label string)`, `Sync([]ClaimState)` all keyed by `Label`.

- [ ] **Step 1: Write the failing test**

```go
func TestRouteTableKeyedByLabel(t *testing.T) {
	tbl := NewRouteTable()
	tbl.Sync([]ClaimState{
		{Label: "8000-sbx1", SandboxID: "sbx1", NodeEndpoint: "10.0.0.7:9091", Port: 8000, Token: "tok", Sharing: "link", Ready: true},
		{Label: "9000-sbx1", SandboxID: "sbx1", NodeEndpoint: "10.0.0.7:9091", Port: 9000, Token: "tok", Sharing: "link", Ready: true},
		{Label: "dead", SandboxID: "sbx2", NodeEndpoint: "x", Port: 1, Token: "t", Ready: false}, // not Ready: dropped
	})
	if r, ok := tbl.Lookup("8000-sbx1"); !ok || r.Port != 8000 || r.SandboxID != "sbx1" || r.NodeEndpoint != "10.0.0.7:9091" {
		t.Fatalf("8000-sbx1 route wrong: %+v ok=%v", r, ok)
	}
	if _, ok := tbl.Lookup("dead"); ok {
		t.Fatal("not-Ready claim must not route")
	}
	// GC: a label absent from the next Sync is reaped.
	tbl.Sync([]ClaimState{{Label: "9000-sbx1", SandboxID: "sbx1", NodeEndpoint: "10.0.0.7:9091", Port: 9000, Token: "tok", Ready: true}})
	if _, ok := tbl.Lookup("8000-sbx1"); ok {
		t.Fatal("8000-sbx1 should be reaped after Sync without it")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/preview/ -run TestRouteTableKeyedByLabel -v`
Expected: FAIL (Route/ClaimState lack the new fields; table keyed by SandboxID).

- [ ] **Step 3: Implement**

Rewrite the `Route` and `ClaimState` structs and re-key the table by `Label` in `internal/preview/route.go`:

```go
// Route is a single expose backend entry: the opaque subdomain label, the
// sandbox it serves, the owning forkd node HTTP endpoint (host:port of :9091),
// the guest port, the per-sandbox bearer the proxy presents to forkd, and the
// access tier. Token is a secret and is never logged.
type Route struct {
	Label        string
	SandboxID    string
	NodeEndpoint string // forkd :9091 host:port (Sandbox.Status.Endpoint)
	Port         int    // guest port
	Token        string // per-sandbox bearer
	Sharing      string // access tier; slice 2a routes "link"
}

// ClaimState is the injectable view a route-sync source maps onto. The slice-2b
// controller reconciler maps a Ready Sandbox (Status.Phase==Ready,
// Status.Endpoint, the <name>-sandbox-token Secret) onto this shape.
type ClaimState struct {
	Label        string
	SandboxID    string
	NodeEndpoint string
	Port         int
	Token        string
	Sharing      string
	Ready        bool
}
```

Re-key `routes map[string]Route` by label, and update `Upsert` (key `r.Label`), `Remove(label string)`, `Lookup(label string)`, and `Sync` (build `want` keyed by `c.Label`, skip `!c.Ready || c.NodeEndpoint == ""`). Keep the same GC semantics.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/preview/ -run 'TestRouteTable' -v`
Expected: PASS. Update any other route tests that constructed `Route{Backend: ...}`.

- [ ] **Step 5: Commit**

```bash
git add internal/preview/route.go internal/preview/route_test.go
git commit -s -m "feat(preview): route carries forkd node endpoint, sandbox id, and port"
```

---

### Task 3: proxy resolves the label and reverse-proxies to the forkd expose handler

The proxy ServeHTTP pipeline becomes: parse `<label>.<domain>`, reject reserved labels, verify the signed link, look up the route, then reverse-proxy to `http://<NodeEndpoint>/v1/sandboxes/<SandboxID>/expose/<Port>/<sub-path>` with the per-sandbox bearer attached and the inbound Authorization and the preview token query stripped. SSE-safe (`FlushInterval = -1`).

**Files:**
- Modify: `internal/preview/proxy.go` (`ServeHTTP`, the director/rewrite, the upstream URL)
- Test: `internal/preview/proxy_test.go`

**Interfaces:**
- Consumes: `ParseHost`, `IsReservedLabel` (Task 1); `RouteTable.Lookup` and the new `Route` (Task 2); the existing `Signer.Verify`.
- The signed link continues to bind `(SandboxID, Port)`; the proxy requires the verified token's sandbox id and port to equal the resolved route's, so a leaked link cannot be replayed against another label.

- [ ] **Step 1: Write the failing test**

```go
// internal/preview/proxy_test.go
func TestProxyRoutesToForkdExposeBackend(t *testing.T) {
	// A stand-in forkd that asserts the expose path, the bearer, and that the
	// preview token did not leak downstream.
	var gotPath, gotAuth, gotToken string
	forkd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.URL.Query().Get("token")
		_, _ = io.WriteString(w, "ok")
	}))
	defer forkd.Close()

	secret := []byte("0123456789abcdef")
	signer, _ := NewSigner(secret)
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: strings.TrimPrefix(forkd.URL, "http://"), Port: 8000, Token: "per-sandbox-bearer", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})

	tok, _ := signer.Mint("sbx1", 8000, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/app/page?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/sandboxes/sbx1/expose/8000/app/page" {
		t.Fatalf("forkd path=%q", gotPath)
	}
	if gotAuth != "Bearer per-sandbox-bearer" {
		t.Fatalf("forkd auth=%q (must be the per-sandbox bearer)", gotAuth)
	}
	if gotToken != "" {
		t.Fatalf("preview token leaked downstream: %q", gotToken)
	}
}

func TestProxyRejectsReservedLabel(t *testing.T) {
	signer, _ := NewSigner([]byte("0123456789abcdef"))
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: NewRouteTable()})
	req := httptest.NewRequest(http.MethodGet, "https://api.mitos.run/", nil)
	req.Host = "api.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reserved label must 404, got %d", rec.Code)
	}
}

func TestProxyRejectsTokenForWrongSandbox(t *testing.T) {
	signer, _ := NewSigner([]byte("0123456789abcdef"))
	routes := NewRouteTable()
	routes.Upsert(Route{Label: "openclaw", SandboxID: "sbx1", NodeEndpoint: "127.0.0.1:1", Port: 8000, Token: "t", Sharing: "link"})
	p := NewProxy(Config{Domain: "mitos.run", Signer: signer, Routes: routes})
	tok, _ := signer.Mint("OTHER", 8000, time.Now().Add(time.Minute)) // token for a different sandbox
	req := httptest.NewRequest(http.MethodGet, "https://openclaw.mitos.run/?token="+tok, nil)
	req.Host = "openclaw.mitos.run"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token for another sandbox must 403, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/preview/ -run TestProxy -v`
Expected: FAIL (the proxy still parses the old scheme and dials `Route.Backend`).

- [ ] **Step 3: Implement**

Rewrite `ServeHTTP` in `internal/preview/proxy.go`:

```go
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	label, ok := ParseHost(r.Host, p.domain)
	if !ok || IsReservedLabel(label) {
		http.Error(w, "unknown expose host", http.StatusNotFound)
		return
	}
	token := extractToken(r)
	if token == "" {
		http.Error(w, "missing expose token", http.StatusUnauthorized)
		return
	}
	claims, err := p.signer.Verify(token)
	if err != nil {
		p.log.Info("expose token rejected", "label", label, "status", http.StatusUnauthorized)
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}
	route, ok := p.routes.Lookup(label)
	if !ok {
		http.Error(w, "no route for label", http.StatusNotFound)
		return
	}
	// The signed link must name the sandbox and port the label resolves to: a
	// leaked link cannot be replayed against another label.
	if claims.SandboxID != route.SandboxID || claims.Port != route.Port {
		p.log.Info("expose token route mismatch", "label", label, "status", http.StatusForbidden)
		http.Error(w, "token does not authorize this route", http.StatusForbidden)
		return
	}

	target := &url.URL{Scheme: "http", Host: route.NodeEndpoint}
	rp := httputil.NewSingleHostReverseProxy(target)
	bearer := route.Token
	prefix := "/v1/sandboxes/" + route.SandboxID + "/expose/" + strconv.Itoa(route.Port)
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		baseDirector(req)
		// Forkd routes by the path; prepend the slice-1 expose prefix.
		req.URL.Path = prefix + req.URL.Path
		req.Host = route.NodeEndpoint
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		stripQueryParam(req, "token")
	}
	rp.FlushInterval = -1 // SSE-safe
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		p.log.Info("expose backend error", "label", label, "status", http.StatusBadGateway)
		http.Error(w, "expose backend unavailable", http.StatusBadGateway)
	}
	rp.ServeHTTP(w, r)
}
```

Add the `strconv` import. Keep `extractToken` and `stripQueryParam`. Note: `NewSingleHostReverseProxy`'s base director sets `req.URL.Path` to the incoming path joined to the target; setting `req.URL.Path = prefix + req.URL.Path` after it yields the full forkd path. Verify in the test that a request to `/app/page` becomes `/v1/sandboxes/sbx1/expose/8000/app/page`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/preview/ -run TestProxy -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/preview/proxy.go internal/preview/proxy_test.go
git commit -s -m "feat(preview): reverse-proxy to the forkd expose handler by label"
```

---

### Task 4: authenticated admin route-sync endpoint

The proxy gains `POST /internal/routes` so the slice-2b controller reconciler can push the Ready route set. It is gated by an admin bearer from `MITOS_EXPOSE_ADMIN_TOKEN` (constant-time compare, never logged) and calls `RouteTable.Sync`.

**Files:**
- Create: `internal/preview/admin.go` (the handler + auth)
- Modify: `cmd/preview-proxy/main.go` (mount the admin route, read the admin token)
- Test: `internal/preview/admin_test.go`

**Interfaces:**
- Produces: `func NewAdminHandler(routes *RouteTable, adminToken string, log *slog.Logger) http.Handler`. It serves `POST /internal/routes` accepting `{"routes":[ClaimState...]}` and calls `routes.Sync`. Missing/wrong admin bearer is 401; malformed body is 400; success is 204.

- [ ] **Step 1: Write the failing test**

```go
// internal/preview/admin_test.go
func TestAdminRouteSyncRequiresToken(t *testing.T) {
	routes := NewRouteTable()
	h := NewAdminHandler(routes, "admin-secret", nil)
	body := `{"routes":[{"Label":"openclaw","SandboxID":"sbx1","NodeEndpoint":"10.0.0.7:9091","Port":8000,"Token":"t","Sharing":"link","Ready":true}]}`

	// No token: 401, no route synced.
	r := httptest.NewRequest(http.MethodPost, "/internal/routes", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status=%d", w.Code)
	}
	if _, ok := routes.Lookup("openclaw"); ok {
		t.Fatal("route synced without auth")
	}

	// Correct token: 204 and route present.
	r = httptest.NewRequest(http.MethodPost, "/internal/routes", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("authed status=%d body=%s", w.Code, w.Body.String())
	}
	if r2, ok := routes.Lookup("openclaw"); !ok || r2.Port != 8000 {
		t.Fatalf("route not synced: %+v", r2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/preview/ -run TestAdminRouteSync -v`
Expected: FAIL (`NewAdminHandler` undefined).

- [ ] **Step 3: Implement**

```go
// internal/preview/admin.go
package preview

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxAdminBody = 8 << 20 // 8 MiB route-set cap

type adminHandler struct {
	routes *RouteTable
	token  string
	log    *slog.Logger
}

// NewAdminHandler returns the authenticated route-sync endpoint. POST
// /internal/routes with {"routes":[ClaimState...]} replaces the route set
// (RouteTable.Sync). The admin token is a bearer credential, compared in
// constant time and never logged.
func NewAdminHandler(routes *RouteTable, adminToken string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	mux := http.NewServeMux()
	h := &adminHandler{routes: routes, token: adminToken, log: log}
	mux.HandleFunc("POST /internal/routes", h.sync)
	return mux
}

func (h *adminHandler) sync(w http.ResponseWriter, r *http.Request) {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.token == "" || !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(h.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var payload struct {
		Routes []ClaimState `json:"routes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	h.routes.Sync(payload.Routes)
	h.log.Info("route set synced", "count", len(payload.Routes))
	w.WriteHeader(http.StatusNoContent)
}
```

In `cmd/preview-proxy/main.go`, read `MITOS_EXPOSE_ADMIN_TOKEN` and mount the admin handler on a SEPARATE listener or the same mux under `/internal/`. Mount on the same mux is acceptable for this slice: add `mux.Handle("/internal/routes", preview.NewAdminHandler(routes, adminToken, logger))` before the catch-all `mux.Handle("/", proxy)`. If the admin token env var is empty, log that route-sync is disabled (the proxy still serves manually-seeded routes for tests) rather than failing closed, since a dev deployment may seed routes another way; document this.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/preview/ -run TestAdminRouteSync -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/preview/admin.go internal/preview/admin_test.go cmd/preview-proxy/main.go
git commit -s -m "feat(preview): authenticated admin route-sync endpoint for the expose proxy"
```

---

### Task 5: docs and threat-model delta

Document the single-label expose host scheme, the forkd-backend routing, the admin route-sync endpoint, and the plaintext cluster hop, and update the threat-model preview/expose ingress row (section 7c) to the new architecture.

**Files:**
- Modify: `docs/preview-urls.md` (the host scheme and the backend description)
- Modify: `docs/threat-model.md` (section 7c preview ingress row)

**Interfaces:** none (documentation).

- [ ] **Step 1: Update docs/preview-urls.md**

Replace the `<sandbox-id>.preview.<domain>` scheme with `<label>.<expose-domain>` (single label, configurable domain), describe the route now targeting the owning forkd node `http://<node>:9091/v1/sandboxes/{id}/expose/{port}/` with the per-sandbox bearer, the reserved-name blocklist, and the admin route-sync endpoint. Keep the signed-link section; note the full sharing ladder is a follow-up (slice 4). No em/en dashes.

- [ ] **Step 2: Update the threat-model section 7c row**

Reflect: the proxy resolves a single-label subdomain to a route and proxies to the forkd expose handler (not a direct sandbox IP); the per-sandbox bearer crosses the cluster network to forkd `:9091` in cleartext (same trust model as the existing SDK path, the cluster network is the trust boundary, in-cluster TLS for `:9091` is a recorded follow-up); reserved-name and Host-allowlist (the route table) defense; the admin route-sync endpoint is a new authenticated control surface (constant-time admin bearer, never logged); the controller route-sync reconciler is slice 2b and the wildcard/PQ TLS is slice 3.

- [ ] **Step 3: Dash check and commit**

Run: `grep -nP "[\x{2014}\x{2013}]" docs/preview-urls.md docs/threat-model.md` (no new matches).

```bash
git add docs/preview-urls.md docs/threat-model.md
git commit -s -m "docs(expose): single-label scheme, forkd backend, and admin route-sync threat-model delta"
```

---

## Self-review notes

- Spec coverage for slice 2a: single-label `<label>.<expose-domain>` scheme (Task 1), reserved names + Host allowlist via the route table (Tasks 1, 3), route to the forkd expose backend with bearer inject and token-leak prevention (Tasks 2, 3), the admin route-sync seam the controller will drive (Task 4), docs + threat-model (Task 5). Deferred to slice 2b: the controller reconciler that reads Ready Sandboxes and the `<name>-sandbox-token` Secrets and POSTs the route set. Deferred to slice 3: wildcard and post-quantum TLS. Deferred to slice 4: the full sharing ladder including the audience selectors.
- Type consistency: `ParseHost`/`IsReservedLabel` (Task 1) feed the proxy (Task 3); `Route`/`ClaimState` with `Label`,`SandboxID`,`NodeEndpoint`,`Port`,`Token`,`Sharing` (Task 2) are consumed by the proxy (Task 3) and the admin handler (Task 4). The signer `Mint(sandboxID, port, expiry)` / `Verify -> Claims{SandboxID, Port}` is unchanged from issue #126.
- Intentional spec deviations recorded in Global Constraints: no package/binary rename; label is an opaque routing key.
- Security: the proxy never derives the upstream from request input (node endpoint, sandbox id, port come from the route); the preview token is stripped before forwarding; the admin token and bearers are constant-time compared and never logged; reserved labels cannot route. The plaintext `:9091` hop is documented, not hidden.
