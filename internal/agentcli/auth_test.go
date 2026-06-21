package agentcli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeAuthService records calls and returns canned responses so the auth command
// wiring is tested without a live account service or gateway.
type fakeAuthService struct {
	principal Principal
	created   CreatedKey
	listed    []KeyInfo

	whoamiErr error
	createErr error
	listErr   error
	revokeErr error

	// recorded inputs
	lastToken    string
	lastOrg      string
	lastScopes   []string
	lastTTL      time.Duration
	revokedKeyID string
	createCalled bool
	revokeCalled bool
}

func (f *fakeAuthService) WhoAmI(_ context.Context, token string) (Principal, error) {
	f.lastToken = token
	if f.whoamiErr != nil {
		return Principal{}, f.whoamiErr
	}
	return f.principal, nil
}

func (f *fakeAuthService) CreateKey(_ context.Context, token, orgID, name string, scopes []string, ttl time.Duration) (CreatedKey, error) {
	f.createCalled = true
	f.lastToken, f.lastOrg, f.lastScopes, f.lastTTL = token, orgID, scopes, ttl
	if f.createErr != nil {
		return CreatedKey{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeAuthService) ListKeys(_ context.Context, token, orgID string) ([]KeyInfo, error) {
	f.lastToken, f.lastOrg = token, orgID
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

func (f *fakeAuthService) RevokeKey(_ context.Context, token, keyID string) error {
	f.revokeCalled = true
	f.lastToken, f.revokedKeyID = token, keyID
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return nil
}

// fakeAuthBackend is a Backend that also exposes an AuthService so Run can reach
// the auth verbs. It embeds FakeBackend for the lifecycle methods it does not
// exercise here.
type fakeAuthBackend struct {
	*FakeBackend
	auth AuthService
}

func (b *fakeAuthBackend) Auth() AuthService { return b.auth }

// runAuth dispatches an auth command against a fake service with an isolated
// config dir so the login profile does not touch the real home dir.
func runAuth(t *testing.T, svc AuthService, args ...string) (out, errw string, code int) {
	t.Helper()
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	backend := &fakeAuthBackend{FakeBackend: &FakeBackend{}, auth: svc}
	var o, e bytes.Buffer
	code = Run(context.Background(), append([]string{"auth"}, args...), backend, &o, &e)
	return o.String(), e.String(), code
}

func TestAuthLoginValidatesTokenAndPersistsProfile(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	svc := &fakeAuthService{principal: Principal{Email: "dev@example.com", OrgIDs: []string{"org-a", "org-b"}}}
	backend := &fakeAuthBackend{FakeBackend: &FakeBackend{}, auth: svc}
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"auth", "login", "--token", "sess-123"}, backend, &out, &errw)
	if code != 0 {
		t.Fatalf("code = %d, errw = %s", code, errw.String())
	}
	if svc.lastToken != "sess-123" {
		t.Errorf("service saw token %q, want sess-123", svc.lastToken)
	}
	if !strings.Contains(out.String(), "dev@example.com") {
		t.Errorf("login output %q missing email", out.String())
	}
	// The token must never appear in stdout or stderr.
	if strings.Contains(out.String(), "sess-123") || strings.Contains(errw.String(), "sess-123") {
		t.Error("session token leaked into CLI output")
	}
	// A subsequent keys command reuses the persisted profile (default org-a).
	creds, err := readCredentials()
	if err != nil {
		t.Fatalf("readCredentials: %v", err)
	}
	if creds.DefaultOrg != "org-a" {
		t.Errorf("persisted default org = %q, want org-a", creds.DefaultOrg)
	}
}

func TestAuthLoginRequiresToken(t *testing.T) {
	out, errw, code := runAuth(t, &fakeAuthService{}, "login")
	if code != 2 {
		t.Fatalf("code = %d, want 2; out=%q errw=%q", code, out, errw)
	}
	if !strings.Contains(errw, "--token is required") {
		t.Errorf("errw = %q, want a --token required message", errw)
	}
}

func TestAuthLoginRejectsBadToken(t *testing.T) {
	svc := &fakeAuthService{whoamiErr: errors.New("invalid session")}
	out, errw, code := runAuth(t, svc, "login", "--token", "bad-token")
	if code != 1 {
		t.Fatalf("code = %d, want 1; out=%q", code, out)
	}
	if strings.Contains(errw, "bad-token") {
		t.Error("rejected token leaked into error output")
	}
}

func TestAuthKeysCreateShowsRawKeyOnceWithWarning(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	svc := &fakeAuthService{
		principal: Principal{Email: "dev@example.com", OrgIDs: []string{"org-a"}},
		created:   CreatedKey{RawKey: "mitos_live_secretbody", Info: KeyInfo{ID: "key-1", OrgID: "org-a"}},
	}
	backend := &fakeAuthBackend{FakeBackend: &FakeBackend{}, auth: svc}
	// Log in first to persist the profile.
	var lo, le bytes.Buffer
	if code := Run(context.Background(), []string{"auth", "login", "--token", "t"}, backend, &lo, &le); code != 0 {
		t.Fatalf("login failed: %s", le.String())
	}
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"auth", "keys", "create", "--name", "ci", "--scopes", "sandboxes,read"}, backend, &out, &errw)
	if code != 0 {
		t.Fatalf("create code = %d, errw = %s", code, errw.String())
	}
	if !svc.createCalled {
		t.Fatal("CreateKey was not called")
	}
	if svc.lastOrg != "org-a" {
		t.Errorf("create org = %q, want org-a (the login default)", svc.lastOrg)
	}
	if len(svc.lastScopes) != 2 || svc.lastScopes[0] != "sandboxes" || svc.lastScopes[1] != "read" {
		t.Errorf("scopes = %v, want [sandboxes read]", svc.lastScopes)
	}
	if !strings.Contains(out.String(), "mitos_live_secretbody") {
		t.Errorf("raw key not shown: %q", out.String())
	}
	if !strings.Contains(errw.String(), "not be shown again") {
		t.Errorf("missing show-once warning: %q", errw.String())
	}
}

func TestAuthKeysListRendersMaskedPrefixes(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	svc := &fakeAuthService{
		principal: Principal{Email: "dev@example.com", OrgIDs: []string{"org-a"}},
		listed: []KeyInfo{
			{ID: "key-1", Name: "ci", Prefix: "mitos_live_abc123...", Scopes: []string{"sandboxes"}},
			{ID: "key-2", Name: "old", Prefix: "mitos_live_def456...", Scopes: []string{"read"}, Revoked: true},
		},
	}
	backend := &fakeAuthBackend{FakeBackend: &FakeBackend{}, auth: svc}
	var lo, le bytes.Buffer
	Run(context.Background(), []string{"auth", "login", "--token", "t"}, backend, &lo, &le)
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"auth", "keys", "ls"}, backend, &out, &errw)
	if code != 0 {
		t.Fatalf("ls code = %d, errw = %s", code, errw.String())
	}
	body := out.String()
	if !strings.Contains(body, "mitos_live_abc123...") || !strings.Contains(body, "revoked") {
		t.Errorf("ls output missing expected rows: %q", body)
	}
}

func TestAuthKeysRevokeWiresKeyID(t *testing.T) {
	t.Setenv("MITOS_CONFIG_DIR", t.TempDir())
	svc := &fakeAuthService{principal: Principal{Email: "dev@example.com", OrgIDs: []string{"org-a"}}}
	backend := &fakeAuthBackend{FakeBackend: &FakeBackend{}, auth: svc}
	var lo, le bytes.Buffer
	Run(context.Background(), []string{"auth", "login", "--token", "t"}, backend, &lo, &le)
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"auth", "keys", "revoke", "key-9"}, backend, &out, &errw)
	if code != 0 {
		t.Fatalf("revoke code = %d, errw = %s", code, errw.String())
	}
	if !svc.revokeCalled || svc.revokedKeyID != "key-9" {
		t.Errorf("revoke wired key id %q, want key-9", svc.revokedKeyID)
	}
}

func TestAuthKeysRequiresLogin(t *testing.T) {
	// No login profile written: a keys command must report not logged in.
	out, errw, code := runAuth(t, &fakeAuthService{}, "keys", "ls", "--org", "org-a")
	if code != 1 {
		t.Fatalf("code = %d, want 1; out=%q", code, out)
	}
	if !strings.Contains(errw, "not logged in") {
		t.Errorf("errw = %q, want a not-logged-in message", errw)
	}
}

func TestAuthUnknownSubcommand(t *testing.T) {
	_, errw, code := runAuth(t, &fakeAuthService{}, "bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errw, "unknown auth subcommand") {
		t.Errorf("errw = %q", errw)
	}
}
