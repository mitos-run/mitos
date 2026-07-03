package agentcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey is a fake api key in the hosted key shape. It is long enough that the
// mask (first 8 + last 4) reveals no usable secret.
const testKey = "mitos_live_c2VjcmV0LWJvZHktZm9yLXRlc3Rz"

// initEnv isolates the config dir and returns a validator recorder. Every init
// test runs against a temp MITOS_CONFIG_DIR so no test touches the developer's
// real ~/.config/mitos.
func initEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MITOS_CONFIG_DIR", dir)
	return dir
}

// recordingValidator returns an InitDeps.Validate that records the endpoint and
// key it was called with and returns err.
func recordingValidator(gotEndpoint, gotKey *string, err error) func(context.Context, string, string) error {
	return func(_ context.Context, endpoint, key string) error {
		*gotEndpoint = endpoint
		*gotKey = key
		return err
	}
}

func TestInitValidKeySavesConfigAndPrintsNextSteps(t *testing.T) {
	dir := initEnv(t)
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, nil)}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), nil, InitOptions{APIKey: testKey}, deps, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errw.String())
	}
	if gotKey != testKey {
		t.Fatalf("validator got key %q, want the provided key", gotKey)
	}
	if gotEndpoint != DefaultHostedBaseURL {
		t.Fatalf("validator got endpoint %q, want default %q", gotEndpoint, DefaultHostedBaseURL)
	}

	// The config file is written with owner-only permissions and both fields.
	path := filepath.Join(dir, "config.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600 (it holds a secret)", fi.Mode().Perm())
	}
	cfg, ok, err := ReadCLIConfig()
	if err != nil || !ok {
		t.Fatalf("ReadCLIConfig = (%v, %v, %v), want a saved config", cfg, ok, err)
	}
	if cfg.APIKey != testKey || cfg.Endpoint != DefaultHostedBaseURL {
		t.Fatalf("saved config = %+v, want key + default endpoint", cfg)
	}

	// The aha: the output ends with the real hosted next step, and the full key
	// is NEVER echoed (only the mask).
	s := out.String()
	for _, want := range []string{
		"mitos sandbox create --pool python",
		"mitos fork",
		"--count 2",
		maskAPIKey(testKey),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s+errw.String(), testKey) {
		t.Fatalf("full api key echoed in output:\n%s%s", s, errw.String())
	}
}

func TestInitCustomEndpointIsValidatedAndSaved(t *testing.T) {
	initEnv(t)
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, nil)}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), []string{"--server", "https://mitos.example.com"},
		InitOptions{APIKey: testKey}, deps, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errw.String())
	}
	if gotEndpoint != "https://mitos.example.com" {
		t.Fatalf("validator got endpoint %q, want the --server value", gotEndpoint)
	}
	cfg, _, _ := ReadCLIConfig()
	if cfg.Endpoint != "https://mitos.example.com" {
		t.Fatalf("saved endpoint = %q, want the --server value", cfg.Endpoint)
	}
}

func TestInitLocalAPIKeyFlagOverridesDefault(t *testing.T) {
	initEnv(t)
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, nil)}
	var out, errw bytes.Buffer

	flagKey := "mitos_live_ZmxhZy1rZXktd2lucy1vdmVyLWVudg"
	code := CmdInit(context.Background(), []string{"--api-key", flagKey},
		InitOptions{APIKey: testKey}, deps, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotKey != flagKey {
		t.Fatalf("validator got key from defaults, want the local --api-key flag to win")
	}
}

func TestInitRejectedKeyDoesNotSaveAndCarriesRemediation(t *testing.T) {
	dir := initEnv(t)
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, errors.New("status 401: unauthorized"))}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), nil, InitOptions{APIKey: testKey}, deps, &out, &errw)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("a rejected key must not be saved; stat err = %v", err)
	}
	s := errw.String()
	if !strings.Contains(s, "https://mitos.run/keys") {
		t.Errorf("rejection message must say where to get a valid key:\n%s", s)
	}
	if !strings.Contains(s, maskAPIKey(testKey)) {
		t.Errorf("rejection message should name the masked key:\n%s", s)
	}
	if strings.Contains(out.String()+s, testKey) {
		t.Fatalf("full api key echoed in output:\n%s", s)
	}
}

func TestInitNoKeyNonInteractiveGuidesAndExits(t *testing.T) {
	dir := initEnv(t)
	deps := InitDeps{Validate: func(context.Context, string, string) error {
		t.Fatal("validator must not run without a key")
		return nil
	}}
	var out, errw bytes.Buffer

	// ReadKey nil = stdin is not an interactive terminal.
	code := CmdInit(context.Background(), nil, InitOptions{}, deps, &out, &errw)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error)", code)
	}
	combined := out.String() + errw.String()
	for _, want := range []string{"https://mitos.run/keys", "MITOS_API_KEY", "--api-key"} {
		if !strings.Contains(combined, want) {
			t.Errorf("guidance missing %q:\n%s", want, combined)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("no config may be written without a key")
	}
}

func TestInitPromptsForKeyOnTTY(t *testing.T) {
	initEnv(t)
	var gotEndpoint, gotKey string
	deps := InitDeps{
		Validate: recordingValidator(&gotEndpoint, &gotKey, nil),
		// The prompt seam stands in for a no-echo terminal read; the pasted key
		// carries surrounding whitespace to prove it is trimmed.
		ReadKey: func() (string, error) { return "  " + testKey + "\n", nil },
	}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), nil, InitOptions{}, deps, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errw.String())
	}
	if gotKey != testKey {
		t.Fatalf("validator got %q, want the trimmed pasted key", gotKey)
	}
	cfg, ok, _ := ReadCLIConfig()
	if !ok || cfg.APIKey != testKey {
		t.Fatalf("pasted key not saved: %+v ok=%v", cfg, ok)
	}
	if strings.Contains(out.String()+errw.String(), testKey) {
		t.Fatalf("full api key echoed in output")
	}
}

func TestInitPromptEmptyKeyIsUsageError(t *testing.T) {
	initEnv(t)
	deps := InitDeps{
		Validate: func(context.Context, string, string) error {
			t.Fatal("validator must not run on an empty key")
			return nil
		},
		ReadKey: func() (string, error) { return "\n", nil },
	}
	var out, errw bytes.Buffer
	if code := CmdInit(context.Background(), nil, InitOptions{}, deps, &out, &errw); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestInitCheckWithoutConfig(t *testing.T) {
	initEnv(t)
	deps := InitDeps{Validate: func(context.Context, string, string) error { return nil }}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), []string{"--check"}, InitOptions{}, deps, &out, &errw)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "mitos init") {
		t.Errorf("remediation must point at running mitos init:\n%s", errw.String())
	}
}

func TestInitCheckValidConfig(t *testing.T) {
	initEnv(t)
	if err := WriteCLIConfig(CLIConfig{Endpoint: "https://mitos.example.com", APIKey: testKey}); err != nil {
		t.Fatal(err)
	}
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, nil)}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), []string{"--check"}, InitOptions{}, deps, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errw.String())
	}
	if gotEndpoint != "https://mitos.example.com" || gotKey != testKey {
		t.Fatalf("check validated (%q, key ok=%v), want the SAVED config", gotEndpoint, gotKey == testKey)
	}
	s := out.String()
	if !strings.Contains(s, "OK") || !strings.Contains(s, maskAPIKey(testKey)) {
		t.Errorf("check report should state OK with the masked key:\n%s", s)
	}
	if strings.Contains(s+errw.String(), testKey) {
		t.Fatalf("full api key echoed by --check")
	}
}

func TestInitCheckRejectedKey(t *testing.T) {
	initEnv(t)
	if err := WriteCLIConfig(CLIConfig{Endpoint: DefaultHostedBaseURL, APIKey: testKey}); err != nil {
		t.Fatal(err)
	}
	var gotEndpoint, gotKey string
	deps := InitDeps{Validate: recordingValidator(&gotEndpoint, &gotKey, errors.New("status 401: revoked"))}
	var out, errw bytes.Buffer

	code := CmdInit(context.Background(), []string{"--check"}, InitOptions{}, deps, &out, &errw)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	s := errw.String()
	if !strings.Contains(s, "https://mitos.run/keys") || !strings.Contains(s, "mitos init") {
		t.Errorf("check failure must carry remediation (new key + re-run init):\n%s", s)
	}
	if strings.Contains(out.String()+s, testKey) {
		t.Fatalf("full api key echoed by --check failure")
	}
}

func TestReadCLIConfigAbsent(t *testing.T) {
	initEnv(t)
	cfg, ok, err := ReadCLIConfig()
	if err != nil {
		t.Fatalf("absent config must not error: %v", err)
	}
	if ok {
		t.Fatalf("absent config reported present: %+v", cfg)
	}
}

func TestMaskAPIKey(t *testing.T) {
	if got := maskAPIKey(testKey); strings.Contains(got, testKey[8:len(testKey)-4]) {
		t.Fatalf("mask %q leaks the secret body", got)
	}
	if got := maskAPIKey(testKey); !strings.HasPrefix(got, testKey[:8]) || !strings.HasSuffix(got, testKey[len(testKey)-4:]) {
		t.Fatalf("mask %q, want prefix + last 4", got)
	}
	if got := maskAPIKey("short"); got != "****" {
		t.Fatalf("short key mask = %q, want opaque ****", got)
	}
	if got := maskAPIKey(""); got != "****" {
		t.Fatalf("empty key mask = %q, want opaque ****", got)
	}
}

// The pure dispatcher cannot run init (it has no validator or terminal); it must
// say to run it via the mitos binary, mirroring the doctor arrangement.
func TestRunDispatchInitWithoutWiring(t *testing.T) {
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"init"}, nil, &out, &errw)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "mitos binary") {
		t.Errorf("message should point at the mitos binary:\n%s", errw.String())
	}
}

func TestUsageMentionsInit(t *testing.T) {
	var out, errw bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, nil, &out, &errw); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "mitos init") {
		t.Errorf("usage does not mention mitos init:\n%s", out.String())
	}
}
