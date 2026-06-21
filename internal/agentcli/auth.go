package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AuthService is the account surface the `mitos auth` commands dispatch to. It is
// narrow on purpose so the command wiring is unit-tested against a fake without a
// running gateway or account database. The real implementation (a documented
// follow-up) calls the hosted account API; here the CLI verbs are proven against
// FakeAuthService. The browser OAuth login flow is a documented follow-up; this
// slice is token-based.
//
// Every method is org-scoped where it touches resources: CreateKey and ListKeys
// take the org id the caller is acting for, so the CLI cannot be used to reach
// another org's keys (the service rejects a non-member, mirroring the account
// service's membership guard).
type AuthService interface {
	// WhoAmI resolves the principal behind a session token: the account email and
	// the org ids it belongs to. It is how `auth login` validates a token.
	WhoAmI(ctx context.Context, token string) (Principal, error)
	// CreateKey mints a scoped API key for an org. The returned raw key is shown
	// exactly once.
	CreateKey(ctx context.Context, token, orgID, name string, scopes []string, ttl time.Duration) (CreatedKey, error)
	// ListKeys returns key metadata (never a raw value) for an org.
	ListKeys(ctx context.Context, token, orgID string) ([]KeyInfo, error)
	// RevokeKey revokes a key by id.
	RevokeKey(ctx context.Context, token, keyID string) error
}

// authProvider is implemented by a Backend that can also resolve an AuthService.
// It is an optional capability: the cluster backend in this slice does not
// implement it, so `mitos auth` reports no service is configured. The seam lets
// the hosted CLI wire a real account client without changing the Backend
// interface.
type authProvider interface {
	Auth() AuthService
}

// authServiceFor returns the AuthService a backend exposes, or nil if the backend
// is nil or does not implement authProvider.
func authServiceFor(backend Backend) AuthService {
	if ap, ok := backend.(authProvider); ok {
		return ap.Auth()
	}
	return nil
}

// Principal is the resolved identity behind a session token.
type Principal struct {
	Email  string
	OrgIDs []string
}

// CreatedKey is the result of a key creation: the raw key (shown once) and its
// masked metadata.
type CreatedKey struct {
	RawKey string
	Info   KeyInfo
}

// KeyInfo is the displayable, non-secret metadata for an API key.
type KeyInfo struct {
	ID        string
	OrgID     string
	Name      string
	Prefix    string
	Scopes    []string
	CreatedAt time.Time
	ExpiresAt time.Time
	Revoked   bool
}

// credentials is the on-disk login profile written by `auth login`. It stores the
// session token, the resolved email, and the default org. The token is a secret;
// it is written to a 0o600 file and is never logged.
type credentials struct {
	Token      string `json:"token"`
	Email      string `json:"email"`
	DefaultOrg string `json:"default_org"`
}

// credentialsPath returns the path of the login profile. It honors MITOS_CONFIG_DIR
// (used by tests and by users who relocate config) and otherwise uses
// $HOME/.config/mitos/credentials.json.
func credentialsPath() (string, error) {
	if dir := os.Getenv("MITOS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "mitos", "credentials.json"), nil
}

// writeCredentials persists the login profile with owner-only permissions so the
// session token is not world-readable.
func writeCredentials(c credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return fmt.Errorf("create config dir: %w", mkErr)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if wErr := os.WriteFile(path, body, 0o600); wErr != nil {
		return fmt.Errorf("write credentials: %w", wErr)
	}
	return nil
}

// readCredentials loads the login profile, returning a friendly error when the
// user is not logged in.
func readCredentials() (credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentials{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return credentials{}, fmt.Errorf("not logged in: run `mitos auth login --token <token>`")
		}
		return credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var c credentials
	if uErr := json.Unmarshal(body, &c); uErr != nil {
		return credentials{}, fmt.Errorf("parse credentials: %w", uErr)
	}
	return c, nil
}

// cmdAuth dispatches the `auth` subcommands. A nil service means the CLI was not
// built with an account service wired (the cluster backend does not include one
// in this slice), so the subcommands report that rather than panicking.
func cmdAuth(ctx context.Context, args []string, svc AuthService, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "auth: a subcommand is required (login, keys)\n\n%s", usage)
		return 2
	}
	switch args[0] {
	case "login":
		return cmdAuthLogin(ctx, args[1:], svc, out, errw)
	case "whoami":
		return cmdAuthWhoami(ctx, svc, out, errw)
	case "keys":
		return cmdAuthKeys(ctx, args[1:], svc, out, errw)
	default:
		fmt.Fprintf(errw, "unknown auth subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdAuthLogin(ctx context.Context, args []string, svc AuthService, out, errw io.Writer) int {
	fs := newFlagSet("auth login", errw)
	token := fs.String("token", "", "session token (browser login is a follow-up)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintf(errw, "auth login: --token is required (browser-based login is a documented follow-up)\n")
		return 2
	}
	if svc == nil {
		fmt.Fprintln(errw, "auth login: no account service is configured for this backend")
		return 1
	}
	p, err := svc.WhoAmI(ctx, *token)
	if err != nil {
		// Never echo the token in the error.
		fmt.Fprintf(errw, "auth login: token rejected: %v\n", err)
		return 1
	}
	defaultOrg := ""
	if len(p.OrgIDs) > 0 {
		defaultOrg = p.OrgIDs[0]
	}
	if err := writeCredentials(credentials{Token: *token, Email: p.Email, DefaultOrg: defaultOrg}); err != nil {
		fmt.Fprintf(errw, "auth login: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "logged in as %s (default org %s)\n", p.Email, orDash(defaultOrg))
	return 0
}

func cmdAuthWhoami(ctx context.Context, svc AuthService, out, errw io.Writer) int {
	creds, err := readCredentials()
	if err != nil {
		fmt.Fprintf(errw, "auth whoami: %v\n", err)
		return 1
	}
	if svc == nil {
		// Fall back to the cached profile when no live service is wired.
		fmt.Fprintf(out, "%s (default org %s)\n", creds.Email, orDash(creds.DefaultOrg))
		return 0
	}
	p, err := svc.WhoAmI(ctx, creds.Token)
	if err != nil {
		fmt.Fprintf(errw, "auth whoami: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "%s\norgs: %s\n", p.Email, strings.Join(p.OrgIDs, ", "))
	return 0
}

// cmdAuthKeys dispatches the key-management verbs.
func cmdAuthKeys(ctx context.Context, args []string, svc AuthService, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "auth keys: a verb is required (create, ls, revoke)\n\n%s", usage)
		return 2
	}
	if svc == nil {
		fmt.Fprintln(errw, "auth keys: no account service is configured for this backend")
		return 1
	}
	creds, err := readCredentials()
	if err != nil {
		fmt.Fprintf(errw, "auth keys: %v\n", err)
		return 1
	}
	switch args[0] {
	case "create":
		return cmdAuthKeysCreate(ctx, args[1:], svc, creds, out, errw)
	case "ls", "list":
		return cmdAuthKeysList(ctx, args[1:], svc, creds, out, errw)
	case "revoke":
		return cmdAuthKeysRevoke(ctx, args[1:], svc, creds, out, errw)
	default:
		fmt.Fprintf(errw, "unknown auth keys verb %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdAuthKeysCreate(ctx context.Context, args []string, svc AuthService, creds credentials, out, errw io.Writer) int {
	fs := newFlagSet("auth keys create", errw)
	name := fs.String("name", "", "human-readable name for the key")
	org := fs.String("org", creds.DefaultOrg, "organization id (defaults to the logged-in default org)")
	scopes := fs.String("scopes", "sandboxes", "comma-separated scopes")
	ttl := fs.Duration("ttl", 0, "lifetime (0 = never expires)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *org == "" {
		fmt.Fprintln(errw, "auth keys create: an org id is required (--org); none was set in the login profile")
		return 2
	}
	created, err := svc.CreateKey(ctx, creds.Token, *org, *name, splitScopes(*scopes), *ttl)
	if err != nil {
		fmt.Fprintf(errw, "auth keys create: %v\n", err)
		return 1
	}
	// The raw key is shown EXACTLY ONCE. The warning makes that explicit.
	fmt.Fprintf(out, "%s\n", created.RawKey)
	fmt.Fprintln(errw, "store this key now; it will not be shown again")
	return 0
}

func cmdAuthKeysList(ctx context.Context, args []string, svc AuthService, creds credentials, out, errw io.Writer) int {
	fs := newFlagSet("auth keys ls", errw)
	org := fs.String("org", creds.DefaultOrg, "organization id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *org == "" {
		fmt.Fprintln(errw, "auth keys ls: an org id is required (--org)")
		return 2
	}
	keys, err := svc.ListKeys(ctx, creds.Token, *org)
	if err != nil {
		fmt.Fprintf(errw, "auth keys ls: %v\n", err)
		return 1
	}
	fmt.Fprint(out, formatKeyInfos(keys))
	return 0
}

func cmdAuthKeysRevoke(ctx context.Context, args []string, svc AuthService, creds credentials, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintf(errw, "auth keys revoke: a key id is required\n\n%s", usage)
		return 2
	}
	if err := svc.RevokeKey(ctx, creds.Token, args[0]); err != nil {
		fmt.Fprintf(errw, "auth keys revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "revoked %s\n", args[0])
	return 0
}

// splitScopes parses a comma-separated scope list, trimming blanks.
func splitScopes(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// formatKeyInfos renders key metadata as a table. The raw value is never present;
// only the masked prefix is shown.
func formatKeyInfos(keys []KeyInfo) string {
	if len(keys) == 0 {
		return "No API keys found.\n"
	}
	header := []string{"ID", "NAME", "PREFIX", "SCOPES", "STATUS"}
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		status := "active"
		if k.Revoked {
			status = "revoked"
		} else if !k.ExpiresAt.IsZero() && !time.Now().Before(k.ExpiresAt) {
			status = "expired"
		}
		rows = append(rows, []string{
			k.ID,
			orDash(k.Name),
			k.Prefix,
			strings.Join(k.Scopes, ","),
			status,
		})
	}
	return renderTable(header, rows)
}
