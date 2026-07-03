package agentcli

import (
	"context"
	"fmt"
	"io"
)

const usage = `mitos: snapshot-fork sandboxes for AI agents

Usage:
  mitos init [--api-key K] [--check]             set up the hosted CLI: validate
                                                    an api key, save it, print
                                                    the first-fork next step
  mitos run <command> [--pool P] [--timeout N]   create a sandbox, run the
                                                    command, terminate, and exit
                                                    with the command's exit code
  mitos sandbox create [--pool P]                create a sandbox, print its id
  mitos sandbox ls [-n namespace] [-A]           list sandboxes
  mitos sandbox exec <id> <command...>           run a command in a sandbox
  mitos fork <id> [--count N]                    fork a running sandbox into N
                                                    live children, print new ids
  mitos sandbox fork <id> [--count N]            alias of the above
  mitos sandbox terminate <id>                   destroy a sandbox
  mitos ws create|ls|log|diff|fork|revert|rm     workspace lifecycle (git verbs)
  mitos ws bind <id> <workspace>                 bind a sandbox to a workspace
  mitos template build --name N                  build a template from a
    (--dockerfile F | --spec F)                    Dockerfile or declarative spec
  mitos template push <name>                     publish a built template
  mitos auth login --token <token>               log in to the hosted offering
  mitos auth keys create|ls|revoke               manage scoped API keys
  mitos dev up | down                            bring a local kind dev
                                                    cluster up or down
  mitos doctor [-n namespace]                    run install/node preflight
                                                    checks and print remediation

Flags:
  --pool string      pool to create sandboxes from
  --timeout int      exec timeout in seconds (0 = backend default)
  -n string          namespace (ls)
  -A                 all namespaces (ls)
  --count int        number of children to fork (fork; alias --replicas)
  -h, --help         print this help
`

// Run is the testable CLI entry point. It dispatches args (without the program
// name) against backend, writing normal output to out and diagnostics to errw,
// and returns a process exit code:
//
//	0  success (for run: the command's exit code)
//	2  usage error (unknown subcommand, missing argument, bad flag)
//	1  a backend or runtime error
//
// For run, the exit code is the executed command's exit code so callers can
// chain mitos in shell pipelines.
func Run(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return 0
	case "run":
		return cmdRun(ctx, args[1:], backend, out, errw)
	case "sandbox":
		return cmdSandbox(ctx, args[1:], backend, out, errw)
	case "fork":
		// Top-level alias for `sandbox fork` so the homepage one-liner
		// `mitos fork <id> --count N` works verbatim (#311).
		return cmdSandboxFork(ctx, args[1:], backend, out, errw)
	case "ws":
		if backend == nil || backend.Workspace() == nil {
			fmt.Fprint(errw, "ws: this backend does not support workspaces\n")
			return 2
		}
		return cmdWorkspace(ctx, args[1:], backend.Workspace(), out, errw)
	case "template":
		if backend == nil {
			fmt.Fprint(errw, "template: this backend does not support templates\n")
			return 2
		}
		return cmdTemplate(ctx, args[1:], backend.Template(), out, errw)
	case "auth":
		// auth login and key management talk to the hosted account service, not the
		// cluster backend. A backend that also exposes an AuthService (via the
		// authProvider interface) wires it in; otherwise the subcommands report no
		// service is configured.
		return cmdAuth(ctx, args[1:], authServiceFor(backend), out, errw)
	case "dev":
		return cmdDev(ctx, args[1:], out, errw)
	case "init":
		// init needs the environment, the terminal, and a live key validator,
		// which the pure CLI dispatcher does not wire; cmd/mitos intercepts init
		// before agentcli.Run and calls CmdInit with the real seams. Reaching
		// here means init was invoked through a path that did not wire them.
		fmt.Fprint(errw, "init: run via the mitos binary, which wires the key validator and terminal\n")
		return 1
	case "doctor":
		// doctor builds a real node + k8s probe (reads /dev, /proc, and the
		// cluster), which the pure CLI dispatcher does not do; cmd/mitos
		// intercepts doctor before agentcli.Run and runs it with a real probe.
		// Reaching here means doctor was invoked through a path that did not wire
		// the probe, so it reports that and returns nonzero.
		fmt.Fprint(errw, "doctor: run via the mitos binary, which wires the node + cluster probe\n")
		return 1
	default:
		fmt.Fprintf(errw, "unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}
