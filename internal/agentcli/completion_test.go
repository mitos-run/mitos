package agentcli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shells maps the completion argument to the interpreter and its syntax-only
// check flag, so the parse test can run each emitted script under its real
// shell when that shell is installed.
var shells = []struct {
	name    string
	binary  string
	parseer func(path string) *exec.Cmd
}{
	{"bash", "bash", func(p string) *exec.Cmd { return exec.Command("bash", "-n", p) }},
	{"zsh", "zsh", func(p string) *exec.Cmd { return exec.Command("zsh", "-n", p) }},
	{"fish", "fish", func(p string) *exec.Cmd { return exec.Command("fish", "--no-execute", p) }},
}

func TestCompletionEmitsScriptPerShell(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		var out, errw bytes.Buffer
		code := cmdCompletion([]string{sh}, &out, &errw)
		if code != 0 {
			t.Fatalf("%s: exit code = %d, want 0 (stderr=%q)", sh, code, errw.String())
		}
		if out.Len() == 0 {
			t.Fatalf("%s: emitted an empty script", sh)
		}
		if errw.Len() != 0 {
			t.Fatalf("%s: wrote to stderr on success: %q", sh, errw.String())
		}
	}
}

// TestCompletionCoversCommandTree is the drift guard: every verb and every
// subcommand the dispatcher accepts must appear in each emitted script. If a
// new verb is added to completionTree (or should have been), this fails.
func TestCompletionCoversCommandTree(t *testing.T) {
	// Assemble the full token set that completion must mention.
	var tokens []string
	tokens = append(tokens, completionTree.top...)
	for _, subs := range completionTree.subs {
		tokens = append(tokens, subs...)
	}

	for _, sh := range []string{"bash", "zsh", "fish"} {
		var out bytes.Buffer
		if code := cmdCompletion([]string{sh}, &out, &bytes.Buffer{}); code != 0 {
			t.Fatalf("%s: exit %d", sh, code)
		}
		script := out.String()
		for _, tok := range tokens {
			if !strings.Contains(script, tok) {
				t.Errorf("%s script missing token %q", sh, tok)
			}
		}
	}
}

func TestCompletionUnknownShell(t *testing.T) {
	var out, errw bytes.Buffer
	code := cmdCompletion([]string{"powershell"}, &out, &errw)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote a script to stdout for an unknown shell: %q", out.String())
	}
	if !strings.Contains(errw.String(), "unknown shell") {
		t.Fatalf("stderr = %q, want it to mention the unknown shell", errw.String())
	}
}

func TestCompletionMissingArg(t *testing.T) {
	var out, errw bytes.Buffer
	code := cmdCompletion(nil, &out, &errw)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "completion") {
		t.Fatalf("stderr = %q, want usage", errw.String())
	}
}

// TestRunDispatchesCompletionOffline proves completion routes through Run with a
// nil backend (no cluster), the property that lets a shell startup file source
// it without a kubeconfig.
func TestRunDispatchesCompletionOffline(t *testing.T) {
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"completion", "bash"}, nil, &out, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, errw.String())
	}
	if !strings.Contains(out.String(), "complete -F _mitos mitos") {
		t.Fatalf("stdout did not contain the bash completion registration: %q", out.String())
	}
}

// TestCompletionScriptsParse runs each emitted script through its real shell's
// syntax checker. A shell that is not installed on the runner is skipped, so
// the test proves correctness where the shell exists and never fails for
// absence (issue #790: parse under their shells in CI where available).
func TestCompletionScriptsParse(t *testing.T) {
	for _, sh := range shells {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			if _, err := exec.LookPath(sh.binary); err != nil {
				t.Skipf("%s not installed, skipping parse check", sh.binary)
			}
			var out bytes.Buffer
			if code := cmdCompletion([]string{sh.name}, &out, &bytes.Buffer{}); code != 0 {
				t.Fatalf("emit exit %d", code)
			}
			path := filepath.Join(t.TempDir(), "mitos-completion")
			if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
				t.Fatalf("write script: %v", err)
			}
			if combined, err := sh.parseer(path).CombinedOutput(); err != nil {
				t.Fatalf("%s failed to parse the emitted script: %v\n%s", sh.binary, err, combined)
			}
		})
	}
}
