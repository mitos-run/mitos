package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// initcmd implements `mitos init`, the hosted-surface first-run verb: it takes a
// new user from key-in-hand to a verified working setup in one command. It
// validates an api key against the gateway (the cheapest authenticated call,
// listing sandboxes), persists endpoint + key to the CLI config file, and ends
// with the real next step (create a sandbox, fork it). `mitos doctor` is the
// cluster-mode counterpart; `mitos init --check` re-validates the saved config.
//
// Security: the api key is a secret. It is written only to the 0o600 config
// file, is never logged, and is never echoed back in full; every message shows
// at most the mask (prefix + last 4). The interactive paste path reads without
// echo (the terminal read is wired by cmd/mitos).

// CLIConfig is the on-disk CLI configuration written by `mitos init`. It lives
// next to the auth credentials (config.json in the same directory) and holds
// the hosted endpoint and the api key. The key is a secret: the file is written
// 0o600 and its value is never logged.
type CLIConfig struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
}

// configFilePath returns the path of the CLI config file, honoring
// MITOS_CONFIG_DIR (tests, relocated config) and otherwise using
// $HOME/.config/mitos/config.json, the same directory as credentials.json.
func configFilePath() (string, error) {
	if dir := os.Getenv("MITOS_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "mitos", "config.json"), nil
}

// WriteCLIConfig persists the config with owner-only permissions so the api key
// is not world-readable.
func WriteCLIConfig(c CLIConfig) error {
	path, err := configFilePath()
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return fmt.Errorf("create config dir: %w", mkErr)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if wErr := os.WriteFile(path, body, 0o600); wErr != nil {
		return fmt.Errorf("write config: %w", wErr)
	}
	return nil
}

// ReadCLIConfig loads the config written by `mitos init`. An absent file is not
// an error: it returns ok=false so callers can fall through (the CLI reads the
// file only as the LAST fallback, after the --api-key/--server flags and the
// MITOS_API_KEY/MITOS_BASE_URL environment variables).
func ReadCLIConfig() (cfg CLIConfig, ok bool, err error) {
	path, err := configFilePath()
	if err != nil {
		return CLIConfig{}, false, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CLIConfig{}, false, nil
		}
		return CLIConfig{}, false, fmt.Errorf("read config: %w", err)
	}
	if uErr := json.Unmarshal(body, &cfg); uErr != nil {
		return CLIConfig{}, false, fmt.Errorf("parse config %s: %w", path, uErr)
	}
	return cfg, true, nil
}

// InitOptions carries the defaults the caller resolved before dispatch: the api
// key and endpoint from the global flags or the environment (flag wins over
// env, both win over the init-local flags' absence).
type InitOptions struct {
	// APIKey is the key from --api-key or MITOS_API_KEY; empty means none.
	APIKey string
	// Endpoint is the base URL from --server or MITOS_BASE_URL; empty means the
	// default hosted endpoint.
	Endpoint string
}

// InitDeps are the injectable seams of `mitos init`, so the command logic is
// unit-tested without a terminal or a live gateway.
type InitDeps struct {
	// Validate checks that apiKey authenticates against endpoint. The real
	// implementation (wired by cmd/mitos) issues the cheapest authenticated
	// call the gateway serves, GET /v1/sandboxes. The error it returns must
	// never contain the key (HostedBackend redacts it).
	Validate func(ctx context.Context, endpoint, apiKey string) error
	// ReadKey reads a pasted api key from the terminal WITHOUT echo. A nil
	// ReadKey means stdin is not an interactive terminal, so init cannot
	// prompt and instead prints where to get a key and how to pass it.
	ReadKey func() (string, error)
}

// maskAPIKey returns the only form of an api key that may ever be printed: the
// first 8 characters plus the last 4. A key too short for that to be safe is
// fully opaque.
func maskAPIKey(key string) string {
	if len(key) < 16 {
		return "****"
	}
	return key[:8] + "..." + key[len(key)-4:]
}

// keysHint says where to get an api key for endpoint: the hosted console keys
// page for the default endpoint, or the deployment's own console for a
// self-hosted or custom endpoint (the console serves the same /keys page in
// both editions).
func keysHint(endpoint string) string {
	if endpoint == DefaultHostedBaseURL {
		return "https://mitos.run/keys"
	}
	return "your deployment's console, page /keys (hosted: https://mitos.run/keys)"
}

// CmdInit implements `mitos init [--api-key K] [--server URL] [--check]`.
// Without --check it validates the key (flag, env default, or interactive
// paste), saves endpoint + key to the config file, and prints the first-fork
// next step. With --check it re-validates the SAVED config and reports. Exit
// codes follow the CLI convention: 0 success, 1 runtime/validation failure,
// 2 usage error (no key available, bad flag).
func CmdInit(ctx context.Context, args []string, defaults InitOptions, deps InitDeps, out, errw io.Writer) int {
	fs := newFlagSet("init", errw)
	apiKey := fs.String("api-key", defaults.APIKey, "api key to validate and save (default: MITOS_API_KEY)")
	server := fs.String("server", defaults.Endpoint, "api endpoint (default: MITOS_BASE_URL, else the hosted endpoint)")
	check := fs.Bool("check", false, "re-validate the saved config and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *check {
		return initCheck(ctx, deps, out, errw)
	}

	endpoint := *server
	if endpoint == "" {
		endpoint = DefaultHostedBaseURL
	}

	key := strings.TrimSpace(*apiKey)
	if key == "" {
		fmt.Fprintf(out, "mitos init sets up the hosted CLI: it validates an api key and saves it for every later command.\n\nNo api key found. Get one at %s (sign up first if you have no account; the free tier needs no card).\n", keysHint(endpoint))
		if deps.ReadKey == nil {
			fmt.Fprintf(errw, "init: stdin is not an interactive terminal, so the key cannot be pasted here. Set MITOS_API_KEY, or run: mitos init --api-key <key>\n")
			return 2
		}
		fmt.Fprint(out, "\nPaste your api key (input is hidden): ")
		pasted, err := deps.ReadKey()
		fmt.Fprintln(out)
		if err != nil {
			fmt.Fprintf(errw, "init: read key: %v\n", err)
			return 1
		}
		key = strings.TrimSpace(pasted)
		if key == "" {
			fmt.Fprintf(errw, "init: no key entered. Set MITOS_API_KEY, or run: mitos init --api-key <key>\n")
			return 2
		}
	}

	if err := deps.Validate(ctx, endpoint, key); err != nil {
		fmt.Fprintf(errw, "init: the key %s was rejected by %s: %v\nCheck that the key is active (not revoked or expired) and mint a new one at %s if needed, then run mitos init again.\n", maskAPIKey(key), endpoint, err, keysHint(endpoint))
		return 1
	}

	if err := WriteCLIConfig(CLIConfig{Endpoint: endpoint, APIKey: key}); err != nil {
		fmt.Fprintf(errw, "init: %v\n", err)
		return 1
	}
	path, _ := configFilePath()
	fmt.Fprintf(out, "init: key %s verified against %s\nsaved to %s; the CLI reads it whenever --api-key and MITOS_API_KEY are unset\n\nYou are ready. Create a sandbox and fork it:\n\n  mitos sandbox create --pool python   # create a sandbox, prints its id\n  mitos fork <id> --count 2            # fork it into 2 live siblings\n\nVerify this setup anytime with: mitos init --check\n", maskAPIKey(key), endpoint, path)
	return 0
}

// initCheck re-validates the SAVED config: it is the hosted counterpart of the
// `mitos doctor` preflight. It reads only the config file (not the environment)
// so it reports on exactly what a flag-less, env-less invocation would use.
func initCheck(ctx context.Context, deps InitDeps, out, errw io.Writer) int {
	cfg, ok, err := ReadCLIConfig()
	if err != nil {
		fmt.Fprintf(errw, "init --check: %v\n", err)
		return 1
	}
	path, _ := configFilePath()
	if !ok {
		fmt.Fprintf(errw, "init --check: no saved config at %s. Run mitos init (with MITOS_API_KEY set, or interactively) to create one.\n", path)
		return 2
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultHostedBaseURL
	}
	if cfg.APIKey == "" {
		fmt.Fprintf(errw, "init --check: the config at %s has no api key. Run mitos init to validate and save one.\n", path)
		return 1
	}
	if err := deps.Validate(ctx, endpoint, cfg.APIKey); err != nil {
		fmt.Fprintf(errw, "init --check: the saved key %s was rejected by %s: %v\nMint a new key at %s and run mitos init again.\n", maskAPIKey(cfg.APIKey), endpoint, err, keysHint(endpoint))
		return 1
	}
	fmt.Fprintf(out, "init --check: OK. Key %s is valid against %s (config %s).\n", maskAPIKey(cfg.APIKey), endpoint, path)
	return 0
}
