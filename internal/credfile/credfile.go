// Package credfile reads the bearer token from the CLI login profile written by
// `mitos auth login`, so the agent-facing surfaces (mcp server, and any other Go
// consumer) pick up one login without a separate env var.
//
// The path rule is the single source of truth shared with the CLI
// (internal/agentcli/auth.go credentialsPath): MITOS_CONFIG_DIR if set, else
// $HOME/.config/mitos/credentials.json. The token VALUE is never logged.
package credfile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// credentials mirrors the on-disk login profile fields this package needs. Only
// the bearer token is read here; the rest of the profile is ignored.
type credentials struct {
	Token string `json:"token"`
}

// Path returns the location of the CLI login profile. It honors MITOS_CONFIG_DIR
// and otherwise uses $HOME/.config/mitos/credentials.json. It returns an empty
// string when no home directory can be resolved, in which case there is simply
// no credential-file fallback.
func Path() string {
	if dir := os.Getenv("MITOS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "mitos", "credentials.json")
}

// Token returns the bearer token from the CLI login profile, or an empty string
// when there is none. A missing, unreadable, or non-JSON file is NOT an error:
// it just yields an empty token so callers stay usable tokenless. The error
// return is reserved for genuinely unexpected failures, of which there are none
// today; it is part of the signature so a future stricter mode can surface one
// without a breaking change. The token VALUE is never logged.
func Token() (string, error) {
	path := Path()
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// Missing or unreadable file: no token, no error.
		return "", nil
	}
	var c credentials
	if err := json.Unmarshal(body, &c); err != nil {
		// Corrupt or non-JSON file: no token, no error.
		return "", nil
	}
	return c.Token, nil
}
