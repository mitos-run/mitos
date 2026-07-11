package agentcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

const wsUsage = `mitos ws: durable, forkable agent workspaces (git verbs)

Usage:
  mitos ws create <name>                  create an empty workspace
  mitos ws ls [-n namespace]              list workspaces
  mitos ws log <workspace>                list revisions, newest first
  mitos ws diff <workspace> <revision>    content-hash diff vs the parent head
  mitos ws fork <src-ws> <revision> <dst-ws>
                                          branch a committed revision into dst-ws
  mitos ws revert <workspace> <revision>  set the workspace head to a past revision
  mitos ws rm <name>                      delete a workspace and its revisions
  mitos ws bind <sandbox-id> <workspace>  bind a running sandbox to a workspace
  mitos ws serve <workspace> --pool P [--port N] [--sharing S] [--as L] [--expose-domain D]
                                          expose a workspace over the Mitos edge proxy
`

// cmdWorkspace is the production entry: it is wired from Run with a cluster
// WorkspaceBackend. runWs is the testable core.
func cmdWorkspace(ctx context.Context, args []string, b WorkspaceBackend, out, errw io.Writer) int {
	return runWs(ctx, args, b, out, errw)
}

func runWs(ctx context.Context, args []string, b WorkspaceBackend, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, wsUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(out, wsUsage)
		return 0
	case "create":
		if len(args) < 2 {
			fmt.Fprint(errw, "ws create: a workspace name is required\n\n"+wsUsage)
			return 2
		}
		if err := b.CreateWorkspace(ctx, args[1]); err != nil {
			fmt.Fprintf(errw, "ws create: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, args[1])
		return 0
	case "ls":
		jsonOut, rest, ferr := extractOutputFlag(args[1:])
		if ferr != nil {
			fmt.Fprintf(errw, "ws ls: %v\n", ferr)
			return ExitUsage
		}
		ns := parseNamespace(rest)
		infos, err := b.ListWorkspaces(ctx, ns)
		if err != nil {
			fmt.Fprintf(errw, "ws ls: %v\n", err)
			return exitCodeFor(err)
		}
		return renderList(out, errw, "ws ls", jsonOut,
			func() (string, error) { return jsonWorkspaceInfos(infos) },
			func() string { return formatWorkspaceList(infos) })
	case "log":
		jsonOut, rest, ferr := extractOutputFlag(args[1:])
		if ferr != nil {
			fmt.Fprintf(errw, "ws log: %v\n", ferr)
			return ExitUsage
		}
		if len(rest) < 1 {
			fmt.Fprint(errw, "ws log: a workspace is required\n\n"+wsUsage)
			return ExitUsage
		}
		revs, err := b.Log(ctx, rest[0])
		if err != nil {
			fmt.Fprintf(errw, "ws log: %v\n", err)
			return exitCodeFor(err)
		}
		return renderList(out, errw, "ws log", jsonOut,
			func() (string, error) { return jsonRevisionLog(revs) },
			func() string { return formatRevisionLog(revs) })
	case "diff":
		if len(args) < 3 {
			fmt.Fprint(errw, "ws diff: a workspace and a revision are required\n\n"+wsUsage)
			return 2
		}
		d, err := b.Diff(ctx, args[1], args[2])
		if err != nil {
			fmt.Fprintf(errw, "ws diff: %v\n", err)
			return 1
		}
		fmt.Fprint(out, formatDiff(d))
		return 0
	case "fork":
		if len(args) < 4 {
			fmt.Fprint(errw, "ws fork: src-workspace, revision, and dst-workspace are required\n\n"+wsUsage)
			return 2
		}
		rev, err := b.Fork(ctx, args[1], args[2], args[3])
		if err != nil {
			fmt.Fprintf(errw, "ws fork: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, rev)
		return 0
	case "revert":
		if len(args) < 3 {
			fmt.Fprint(errw, "ws revert: a workspace and a revision are required\n\n"+wsUsage)
			return 2
		}
		rev, err := b.Revert(ctx, args[1], args[2])
		if err != nil {
			fmt.Fprintf(errw, "ws revert: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, rev)
		return 0
	case "rm":
		if len(args) < 2 {
			fmt.Fprint(errw, "ws rm: a workspace name is required\n\n"+wsUsage)
			return 2
		}
		if err := b.RemoveWorkspace(ctx, args[1]); err != nil {
			fmt.Fprintf(errw, "ws rm: %v\n", err)
			return 1
		}
		return 0
	case "bind":
		if len(args) < 3 {
			fmt.Fprint(errw, "ws bind: a sandbox id and a workspace are required\n\n"+wsUsage)
			return 2
		}
		if err := b.Bind(ctx, args[1], args[2]); err != nil {
			fmt.Fprintf(errw, "ws bind: %v\n", err)
			return 1
		}
		return 0
	case "serve":
		return cmdServe(ctx, args[1:], b, out, errw)
	default:
		fmt.Fprintf(errw, "unknown ws subcommand %q\n\n%s", args[0], wsUsage)
		return 2
	}
}

// parseNamespace pulls -n/--namespace out of the remaining args.
func parseNamespace(args []string) string {
	for i := 0; i < len(args); i++ {
		if (args[i] == "-n" || args[i] == "--namespace") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func formatWorkspaceList(infos []WorkspaceInfo) string {
	if len(infos) == 0 {
		return "no workspaces\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-24s %-24s %-9s %s\n", "NAME", "HEAD", "REVISIONS", "RESUMABLE")
	for _, w := range infos {
		fmt.Fprintf(&sb, "%-24s %-24s %-9d %t\n", w.Name, w.Head, w.Revisions, w.Resumable)
	}
	return sb.String()
}

func formatRevisionLog(revs []RevisionInfo) string {
	if len(revs) == 0 {
		return "no revisions\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-24s %-10s %-9s %s\n", "REVISION", "PHASE", "RESUMABLE", "LINEAGE")
	for _, r := range revs {
		fmt.Fprintf(&sb, "%-24s %-10s %-9t %s\n", r.Name, r.Phase, r.Resumable, r.Lineage)
	}
	return sb.String()
}

// cmdServe handles the `mitos ws serve` subcommand.
func cmdServe(ctx context.Context, args []string, b WorkspaceBackend, out, errw io.Writer) int {
	// Extract the workspace name: it is the first non-flag argument. The Go
	// flag package stops at the first non-flag arg, so we pull the workspace
	// name out before flag parsing when args[0] is not a flag.
	var workspace string
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		workspace = args[0]
		flagArgs = args[1:]
	}

	fs := flag.NewFlagSet("ws serve", flag.ContinueOnError)
	fs.SetOutput(errw)
	pool := fs.String("pool", "", "SandboxPool to start the sandbox from (required)")
	port := fs.Int("port", 8080, "guest TCP port to expose")
	sharing := fs.String("sharing", "private", "access tier: private, link, org, authenticated, or public")
	label := fs.String("as", "", "subdomain label (defaults to the sandbox name)")
	exposeDomain := fs.String("expose-domain", DefaultExposeDomain(), "base domain for the expose URL (or set MITOS_EXPOSE_DOMAIN)")

	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	if workspace == "" {
		// Workspace may also appear after flags (e.g., mitos ws serve --pool P myws).
		if len(fs.Args()) > 0 {
			workspace = fs.Args()[0]
		}
	}

	if workspace == "" {
		fmt.Fprintf(errw, "ws serve: a workspace name is required\n\n%s", wsUsage)
		return 2
	}
	if *pool == "" {
		fmt.Fprintf(errw, "ws serve: --pool is required\n\n%s", wsUsage)
		return 2
	}
	if *exposeDomain == "" {
		fmt.Fprintf(errw, "ws serve: --expose-domain is required (or set MITOS_EXPOSE_DOMAIN)\n")
		return 2
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(errw, "ws serve: --port %d out of range 1-65535\n", *port)
		return 2
	}

	res, err := b.Serve(ctx, workspace, *exposeDomain, ServeOptions{
		Pool:    *pool,
		Port:    *port,
		Sharing: *sharing,
		Label:   *label,
	})
	if err != nil {
		fmt.Fprintf(errw, "ws serve: %v\n", err)
		return 1
	}

	fmt.Fprintln(out, res.URL)
	fmt.Fprintln(out, "Note: this URL is reachable once the expose proxy is deployed and *."+*exposeDomain+" DNS resolves to it.")
	if res.Sharing == "private" {
		fmt.Fprintln(out, "Access requires OIDC login (private sharing).")
	}
	return 0
}

func formatDiff(d DiffInfo) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "parent: %s\n", d.Parent)
	for _, a := range d.Added {
		fmt.Fprintf(&sb, "+ %s\n", a)
	}
	for _, m := range d.Modified {
		fmt.Fprintf(&sb, "~ %s\n", m)
	}
	for _, r := range d.Removed {
		fmt.Fprintf(&sb, "- %s\n", r)
	}
	if len(d.Added)+len(d.Modified)+len(d.Removed) == 0 {
		sb.WriteString("(no changes)\n")
	}
	return sb.String()
}
