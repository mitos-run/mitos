package agentcli

import (
	"fmt"
	"io"
	"strings"
)

// completionCommand is the shell-completion word tree. It is the single source
// of truth for the static scripts emitted by `mitos completion`, so it must
// stay in sync with the dispatch in cli.go and the subcommand files
// (commands.go, workspace_cmd.go, template_cmd.go, auth.go, dev.go).
type completionCommand struct {
	// top is the ordered set of first-position verbs.
	top []string
	// subs maps a verb to its second-position subcommands.
	subs map[string][]string
	// flags is the set of global flags offered once a verb is chosen.
	flags []string
}

// completionTree enumerates every verb the CLI dispatches. When a verb or a
// subcommand is added, extend this tree so completion keeps covering it; the
// completion_test.go coverage test fails loudly if the dispatch and this tree
// drift apart.
var completionTree = completionCommand{
	top: []string{
		"init", "run", "sandbox", "fork", "ws", "template",
		"auth", "dev", "doctor", "version", "completion", "help",
	},
	subs: map[string][]string{
		"sandbox":    {"create", "ls", "exec", "fork", "terminate"},
		"ws":         {"create", "ls", "log", "diff", "fork", "revert", "rm", "bind", "serve"},
		"template":   {"build", "push"},
		"auth":       {"login", "whoami", "keys"},
		"dev":        {"up", "down"},
		"completion": {"bash", "zsh", "fish"},
	},
	// Global flags a user can append after any verb. Kept flat and static: the
	// hand-rolled parser accepts these before or after the subcommand, so
	// offering them broadly is honest without over-promising per-verb specifics.
	flags: []string{
		"--pool", "--timeout", "--namespace", "--count",
		"--api-key", "--server", "--help",
	},
}

// completionUsage is printed when `mitos completion` is called without a valid
// shell argument. It doubles as the install hint for each shell.
const completionUsage = `mitos completion <bash|zsh|fish>

Emit a shell completion script for verbs, subcommands, and flags.

  bash:  mitos completion bash  > /etc/bash_completion.d/mitos
         # or, per-user, add to ~/.bashrc:
         #   source <(mitos completion bash)
  zsh:   mitos completion zsh   > "${fpath[1]}/_mitos"
         # or add to ~/.zshrc:  source <(mitos completion zsh)
  fish:  mitos completion fish  > ~/.config/fish/completions/mitos.fish

The scripts are static: they complete the command tree without contacting a
cluster, so they never block the shell and work offline.
`

// cmdCompletion emits a static shell completion script for the requested shell.
// It contacts no backend so it works offline and cannot block the shell. An
// unknown or missing shell argument is a usage error (exit 2).
func cmdCompletion(args []string, out, errw io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(errw, completionUsage)
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(out, bashCompletion())
		return 0
	case "zsh":
		fmt.Fprint(out, zshCompletion())
		return 0
	case "fish":
		fmt.Fprint(out, fishCompletion())
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(out, completionUsage)
		return 0
	default:
		fmt.Fprintf(errw, "completion: unknown shell %q, want bash, zsh, or fish\n\n%s", args[0], completionUsage)
		return 2
	}
}

// subCases builds the per-shell "verb -> subcommands" branches from the tree so
// the three generators share one source. renderCase formats a single branch.
func subCases(renderCase func(verb string, subs []string) string) string {
	var b strings.Builder
	for _, verb := range completionTree.top {
		subs, ok := completionTree.subs[verb]
		if !ok {
			continue
		}
		b.WriteString(renderCase(verb, subs))
	}
	return b.String()
}

func bashCompletion() string {
	top := strings.Join(completionTree.top, " ")
	flags := strings.Join(completionTree.flags, " ")
	cases := subCases(func(verb string, subs []string) string {
		return fmt.Sprintf("        %s) opts=%q ;;\n", verb, strings.Join(subs, " "))
	})
	return fmt.Sprintf(`# bash completion for mitos. Load with: source <(mitos completion bash)
_mitos() {
    local cur top flags opts
    cur="${COMP_WORDS[COMP_CWORD]}"
    top=%q
    flags=%q

    if [ "$COMP_CWORD" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$top" -- "$cur") )
        return 0
    fi

    if [ "$COMP_CWORD" -eq 2 ]; then
        opts=""
        case "${COMP_WORDS[1]}" in
%s        esac
        if [ -n "$opts" ]; then
            COMPREPLY=( $(compgen -W "$opts" -- "$cur") )
            return 0
        fi
    fi

    COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
    return 0
}
complete -F _mitos mitos
`, top, flags, cases)
}

func zshCompletion() string {
	top := strings.Join(completionTree.top, " ")
	flags := strings.Join(completionTree.flags, " ")
	cases := subCases(func(verb string, subs []string) string {
		return fmt.Sprintf("        %s) _values 'subcommand' %s ;;\n", verb, strings.Join(subs, " "))
	})
	return fmt.Sprintf(`#compdef mitos
# zsh completion for mitos. Load with: source <(mitos completion zsh)
_mitos() {
    local -a top
    top=(%s)
    if (( CURRENT == 2 )); then
        _describe 'command' top
        return
    fi
    case ${words[2]} in
%s        *) _values 'flag' %s ;;
    esac
}
compdef _mitos mitos
`, top, cases, flags)
}

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# fish completion for mitos. Load with: mitos completion fish > ~/.config/fish/completions/mitos.fish\n")
	b.WriteString("complete -c mitos -f\n")
	b.WriteString(fmt.Sprintf("complete -c mitos -n '__fish_use_subcommand' -a '%s'\n", strings.Join(completionTree.top, " ")))
	for _, verb := range completionTree.top {
		subs, ok := completionTree.subs[verb]
		if !ok {
			continue
		}
		b.WriteString(fmt.Sprintf("complete -c mitos -n '__fish_seen_subcommand_from %s' -a '%s'\n", verb, strings.Join(subs, " ")))
	}
	// Global flags, offered everywhere. Strip the leading -- so fish -l takes the
	// bare long-option name.
	for _, f := range completionTree.flags {
		b.WriteString(fmt.Sprintf("complete -c mitos -l %s\n", strings.TrimLeft(f, "-")))
	}
	return b.String()
}
