package agentcli

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// shells maps the completion argument to the interpreter and its syntax-only
// check flag, so the parse test can run each emitted script under its real
// shell when that shell is installed.
var shells = []struct {
	name   string
	binary string
	parser func(path string) *exec.Cmd
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

// TestCompletionCoversFlags guards the global flag set: every long and short
// flag in completionTree must appear in each emitted script. fish registers
// short flags with -s (the bare char), so assert the bare token for those.
func TestCompletionCoversFlags(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		var out bytes.Buffer
		if code := cmdCompletion([]string{sh}, &out, &bytes.Buffer{}); code != 0 {
			t.Fatalf("%s: exit %d", sh, code)
		}
		script := out.String()
		// bash and zsh keep the leading dashes (--pool, -A); fish registers the
		// bare name with -l (long) or -s (short). Accept either form.
		for _, f := range completionTree.flags {
			bare := strings.TrimLeft(f, "-")
			if !strings.Contains(script, f) && !strings.Contains(script, "-l "+bare) {
				t.Errorf("%s script missing long flag %q", sh, f)
			}
		}
		for _, f := range completionTree.shortFlags {
			bare := strings.TrimLeft(f, "-")
			if !strings.Contains(script, f) && !strings.Contains(script, "-s "+bare) {
				t.Errorf("%s script missing short flag %q", sh, f)
			}
		}
	}
}

// TestCompletionTreeMatchesDispatch is the true drift guard. It parses the
// agentcli dispatch switches from source and asserts every verb and subcommand
// the CLI actually dispatches is covered by completionTree. Adding a case to a
// dispatch switch without extending the tree fails here, which the script-token
// tests above cannot catch (they only prove the tree's own tokens reach the
// emitted scripts). The `version` verb and the offline verbs are intercepted in
// cmd/mitos/main.go before the backend is built; they live in the tree and are
// exercised by the script tests, so this in-package guard focuses on the
// dispatch switches most likely to grow new subcommands.
func TestCompletionTreeMatchesDispatch(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse agentcli package: %v", err)
	}
	pkg := pkgs["agentcli"]
	if pkg == nil {
		t.Fatalf("agentcli package not found in parse result")
	}

	// firstSwitchCases returns the string-literal case tokens of the first
	// switch statement in the named function, which is the subcommand
	// dispatcher (this codebase dispatches first, then handles). A found guard
	// stops collection after that switch so later flag switches are ignored.
	firstSwitchCases := func(fnName string) []string {
		var cases []string
		found := false
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Name.Name != fnName || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if found {
						return false
					}
					sw, ok := n.(*ast.SwitchStmt)
					if !ok {
						return true
					}
					found = true
					for _, stmt := range sw.Body.List {
						cc, ok := stmt.(*ast.CaseClause)
						if !ok {
							continue
						}
						for _, expr := range cc.List {
							lit, ok := expr.(*ast.BasicLit)
							if !ok || lit.Kind != token.STRING {
								continue
							}
							if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
								cases = append(cases, v)
							}
						}
					}
					return false
				})
			}
		}
		return cases
	}

	inTop := func(tok string) bool {
		for _, v := range completionTree.top {
			if v == tok {
				return true
			}
		}
		return false
	}
	inSubs := func(verb, tok string) bool {
		for _, s := range completionTree.subs[verb] {
			if s == tok {
				return true
			}
		}
		return false
	}
	// skip lists tokens that are intentionally not distinct completion words:
	// help aliases, the `list` alias of `ls`, and any flag-like token.
	skip := func(tok string) bool {
		return tok == "help" || tok == "list" || strings.HasPrefix(tok, "-")
	}

	topCases := firstSwitchCases("Run")
	if len(topCases) == 0 {
		t.Fatalf("no top-level dispatch cases found for Run; parse heuristic broke")
	}
	for _, tok := range topCases {
		if skip(tok) {
			continue
		}
		if !inTop(tok) {
			t.Errorf("cli.go Run dispatches verb %q but completionTree.top omits it (stale completion)", tok)
		}
	}

	// subDispatch maps a subcommand dispatch function to the tree verb it
	// serves. cmdAuthKeys is a third level (auth keys ...) the two-level tree
	// does not model, so it is deliberately not checked here.
	subDispatch := map[string]string{
		"cmdSandbox":  "sandbox",
		"runWs":       "ws",
		"cmdTemplate": "template",
		"cmdAuth":     "auth",
		"cmdDev":      "dev",
	}
	for fn, verb := range subDispatch {
		cases := firstSwitchCases(fn)
		if len(cases) == 0 {
			t.Errorf("no dispatch switch cases found for %s; parse heuristic broke", fn)
			continue
		}
		for _, tok := range cases {
			if skip(tok) {
				continue
			}
			if !inSubs(verb, tok) {
				t.Errorf("%s dispatches subcommand %q but completionTree.subs[%q] omits it (stale completion)", fn, tok, verb)
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
			if combined, err := sh.parser(path).CombinedOutput(); err != nil {
				t.Fatalf("%s failed to parse the emitted script: %v\n%s", sh.binary, err, combined)
			}
		})
	}
}
