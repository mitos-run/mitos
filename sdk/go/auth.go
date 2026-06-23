package mitos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL is the hosted production control plane. When neither the
// WithBaseURL option nor MITOS_BASE_URL is set, the client targets this hosted
// endpoint so the examples work without a base URL. Self-hosted or local
// standalone users opt out by setting MITOS_BASE_URL (for example
// http://localhost:8080). It mirrors the Python, TypeScript, Ruby, Rust, and
// Java SDK defaults.
const DefaultBaseURL = "https://mitos.run"

// Environment variables for the direct-mode onboarding path. Explicit options
// always take precedence over these.
const (
	envAPIKey    = "MITOS_API_KEY"
	envBaseURL   = "MITOS_BASE_URL"
	envConfigDir = "MITOS_CONFIG_DIR"
)

// resolveBaseURL applies the base-URL precedence: the explicit value, then
// MITOS_BASE_URL, then the hosted production endpoint. Trailing slashes are
// stripped. Parity with the other SDKs' base-URL resolution.
func resolveBaseURL(url string) string {
	chosen := url
	if chosen == "" {
		chosen = os.Getenv(envBaseURL)
	}
	if chosen == "" {
		chosen = DefaultBaseURL
	}
	return strings.TrimRight(chosen, "/")
}

// resolveToken applies the bearer precedence: the explicit value, then
// MITOS_API_KEY, then the bearer token in the CLI login credential file written
// by `mitos auth login` (so one login authenticates the SDK too), then the empty
// string (tokenless). The file token is sent as-is and the gateway decides its
// validity. The standalone server is tokenless and ignores the token; the hosted
// front door verifies it. The token VALUE is never logged.
func resolveToken(apiKey string) string {
	if apiKey != "" {
		return apiKey
	}
	if env := os.Getenv(envAPIKey); env != "" {
		return env
	}
	return tokenFromCredentialFile()
}

// credentialsPath returns the location of the CLI login profile written by
// `mitos auth login`. It honors MITOS_CONFIG_DIR and otherwise uses
// $HOME/.config/mitos/credentials.json. It returns an empty string when no home
// directory can be resolved, in which case there is simply no credential-file
// fallback. The path rule mirrors internal/credfile, the single source of truth
// shared with the CLI.
func credentialsPath() string {
	if dir := os.Getenv(envConfigDir); dir != "" {
		return filepath.Join(dir, "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "mitos", "credentials.json")
}

// credentials mirrors the on-disk login profile fields this package needs. Only
// the bearer token is read; the rest of the profile is ignored.
type credentials struct {
	Token string `json:"token"`
}

// tokenFromCredentialFile reads the bearer token from the CLI login profile, or
// the empty string when there is none. A missing, unreadable, or non-JSON file
// (or one without a non-empty "token") is NOT an error: it just yields no token
// so the SDK stays usable tokenless. The token VALUE is never logged.
func tokenFromCredentialFile() string {
	path := credentialsPath()
	if path == "" {
		return ""
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// Missing or unreadable file: no token, no error.
		return ""
	}
	var c credentials
	if err := json.Unmarshal(body, &c); err != nil {
		// Corrupt or non-JSON file: no token, no error.
		return ""
	}
	return c.Token
}
