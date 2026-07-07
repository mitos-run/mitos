package agentcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// newFlagSet builds a flag set that writes its own errors to errw (so a bad flag
// surfaces on the CLI's error stream) and never calls os.Exit.
func newFlagSet(name string, errw io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() {}
	return fs
}

// cmdRun implements `mitos run <command> [--pool P] [--timeout N]`: create a
// sandbox, run the command, terminate the sandbox, and return the command's exit
// code. Terminate runs even when exec fails.
func cmdRun(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("run", errw)
	pool := fs.String("pool", "", "pool to create the sandbox from")
	timeout := fs.Int("timeout", 0, "exec timeout in seconds (0 = backend default)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return ExitUsage
	}
	command := strings.Join(fs.Args(), " ")
	if command == "" {
		fmt.Fprintf(errw, "run: a command is required\n\n%s", usage)
		return ExitUsage
	}

	id, err := backend.Create(ctx, *pool)
	if err != nil {
		fmt.Fprintf(errw, "create sandbox: %v\n", err)
		return exitCodeFor(err)
	}

	result, execErr := backend.Exec(ctx, id, command, *timeout)

	// Always attempt termination, even on exec error, so a sandbox is not
	// leaked. A terminate failure is reported but does not mask the exec
	// outcome.
	if termErr := backend.Terminate(ctx, id); termErr != nil {
		fmt.Fprintf(errw, "terminate sandbox %s: %v\n", id, termErr)
	}

	if execErr != nil {
		fmt.Fprintf(errw, "exec: %v\n", execErr)
		return exitCodeFor(execErr)
	}
	if result.Stdout != "" {
		fmt.Fprint(out, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(errw, result.Stderr)
	}
	return result.ExitCode
}

// cmdSandbox dispatches the `sandbox` subcommands.
func cmdSandbox(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "sandbox: a subcommand is required\n\n%s", usage)
		return ExitUsage
	}
	switch args[0] {
	case "create":
		return cmdSandboxCreate(ctx, args[1:], backend, out, errw)
	case "ls":
		return cmdSandboxLs(ctx, args[1:], backend, out, errw)
	case "exec":
		return cmdSandboxExec(ctx, args[1:], backend, out, errw)
	case "fork":
		return cmdSandboxFork(ctx, args[1:], backend, out, errw)
	case "terminate":
		return cmdSandboxTerminate(ctx, args[1:], backend, out, errw)
	default:
		fmt.Fprintf(errw, "unknown sandbox subcommand %q\n\n%s", args[0], usage)
		return ExitUsage
	}
}

func cmdSandboxCreate(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("sandbox create", errw)
	pool := fs.String("pool", "", "pool to create the sandbox from")
	wait := fs.Bool("wait", true, "wait for the sandbox to become Ready before returning")
	noWait := fs.Bool("no-wait", false, "return as soon as the sandbox is created; do not wait for Ready")
	timeout := fs.Int("timeout", 0, "max seconds to wait for Ready (0 = backend default)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return ExitUsage
	}
	octx, cancel := lifecycleContext(ctx, *wait, *noWait, *timeout)
	defer cancel()
	id, err := backend.Create(octx, *pool)
	if err != nil {
		fmt.Fprintf(errw, "create sandbox: %v\n", err)
		return exitCodeFor(err)
	}
	fmt.Fprintln(out, id)
	return ExitOK
}

func cmdSandboxLs(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	jsonOut, rest, ferr := extractOutputFlag(args)
	if ferr != nil {
		fmt.Fprintf(errw, "sandbox ls: %v\n", ferr)
		return ExitUsage
	}
	fs := newFlagSet("sandbox ls", errw)
	namespace := fs.String("n", "", "namespace")
	allNamespaces := fs.Bool("A", false, "all namespaces")
	if err := fs.Parse(rest); err != nil {
		fmt.Fprint(errw, usage)
		return ExitUsage
	}
	ns := *namespace
	if *allNamespaces {
		ns = ""
	}
	infos, err := backend.List(ctx, ns)
	if err != nil {
		fmt.Fprintf(errw, "list sandboxes: %v\n", err)
		return exitCodeFor(err)
	}
	return renderList(out, errw, "sandbox ls", jsonOut,
		func() (string, error) { return jsonSandboxInfos(infos) },
		func() string { return formatSandboxInfos(infos) })
}

func cmdSandboxExec(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintf(errw, "sandbox exec: <id> and a command are required\n\n%s", usage)
		return ExitUsage
	}
	id := args[0]
	command := strings.Join(args[1:], " ")
	result, err := backend.Exec(ctx, id, command, 0)
	if err != nil {
		fmt.Fprintf(errw, "exec: %v\n", err)
		return exitCodeFor(err)
	}
	if result.Stdout != "" {
		fmt.Fprint(out, result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(errw, result.Stderr)
	}
	return result.ExitCode
}

func cmdSandboxFork(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("sandbox fork", errw)
	// --count is the documented flag (#311); --replicas is kept as a back-compat
	// alias. --count wins when set (default 0 means "not set, use --replicas").
	count := fs.Int("count", 0, "number of forks (alias of --replicas)")
	replicas := fs.Int("replicas", 1, "number of forks")
	wait := fs.Bool("wait", true, "wait for the forks to become Ready before returning")
	noWait := fs.Bool("no-wait", false, "return as soon as the forks are created; do not wait for Ready")
	timeout := fs.Int("timeout", 0, "max seconds to wait for Ready (0 = backend default)")
	// Accept the sandbox id either before or after the flags, so both
	// `fork sbx-1 --count 3` and `fork --count 3 sbx-1` parse correctly.
	id, err := parseLeadingID(fs, args)
	if err != nil {
		fmt.Fprint(errw, usage)
		return ExitUsage
	}
	if id == "" {
		fmt.Fprintf(errw, "sandbox fork: a sandbox id is required\n\n%s", usage)
		return ExitUsage
	}
	n := *replicas
	if *count > 0 {
		n = *count
	}
	octx, cancel := lifecycleContext(ctx, *wait, *noWait, *timeout)
	defer cancel()
	ids, err := backend.Fork(octx, id, n)
	if err != nil {
		fmt.Fprintf(errw, "fork: %v\n", err)
		return exitCodeFor(err)
	}
	for _, fid := range ids {
		fmt.Fprintln(out, fid)
	}
	return ExitOK
}

func cmdSandboxTerminate(ctx context.Context, args []string, backend Backend, out, errw io.Writer) int {
	fs := newFlagSet("sandbox terminate", errw)
	// terminate is asynchronous: the object is deleted and the controller reaps
	// it. --timeout bounds the delete call itself; waiting until the sandbox is
	// fully reaped is a named follow-up, so --wait is not offered here yet.
	timeout := fs.Int("timeout", 0, "max seconds to bound the delete call (0 = no bound)")
	id, err := parseLeadingID(fs, args)
	if err != nil {
		fmt.Fprint(errw, usage)
		return ExitUsage
	}
	if id == "" {
		fmt.Fprintf(errw, "sandbox terminate: a sandbox id is required\n\n%s", usage)
		return ExitUsage
	}
	octx, cancel := lifecycleContext(ctx, true, false, *timeout)
	defer cancel()
	if err := backend.Terminate(octx, id); err != nil {
		fmt.Fprintf(errw, "terminate: %v\n", err)
		return exitCodeFor(err)
	}
	fmt.Fprintf(out, "terminated %s\n", id)
	return ExitOK
}

// cmdDev validates the `dev up|down` arguments. The dev orchestration shells out
// to kind and kubectl, which the pure CLI dispatcher does not do; cmd/mitos
// intercepts the dev subcommand before agentcli.Run and runs DevUp/DevDown with
// a real exec runner. Reaching cmdDev means dev was invoked through a path that
// did not wire the runner, so it reports that and returns nonzero.
func cmdDev(_ context.Context, args []string, _, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(errw, "dev: 'up' or 'down' is required\n\n%s", usage)
		return ExitUsage
	}
	switch args[0] {
	case "up", "down":
		fmt.Fprintf(errw, "dev %s: run via the mitos binary, which wires the kind/kubectl runner\n", args[0])
		return ExitError
	default:
		fmt.Fprintf(errw, "unknown dev subcommand %q\n\n%s", args[0], usage)
		return ExitUsage
	}
}

// parseLeadingID parses fs from args when the command takes a single positional
// id that may appear either before or after its flags. The stdlib flag parser
// stops at the first non-flag token, so a single parse mis-reads a flag value as
// the id (`fork --count 2 sbx-1` would take "2"). This parses once to consume any
// leading flags, lifts the first remaining positional as the id, then parses the
// tokens that followed the id so trailing flags (`fork sbx-1 --count 2`) are
// still honored. It returns an empty id when no positional is present.
func parseLeadingID(fs *flag.FlagSet, args []string) (id string, err error) {
	if err = fs.Parse(args); err != nil {
		return "", err
	}
	pos := fs.Args()
	if len(pos) == 0 {
		return "", nil
	}
	id = pos[0]
	if err = fs.Parse(pos[1:]); err != nil {
		return "", err
	}
	return id, nil
}

// formatSandboxInfos renders SandboxInfo rows as an aligned table with columns
// NAME POOL PHASE NODE ENDPOINT AGE. An empty list returns a friendly message.
// The age formatting matches the kubectl-style rendering used by the
// kubectl-mitos plugin (single largest unit).
func formatSandboxInfos(infos []SandboxInfo) string {
	if len(infos) == 0 {
		return "No sandboxes found.\n"
	}
	header := []string{"NAME", "POOL", "PHASE", "NODE", "ENDPOINT", "AGE"}
	rows := make([][]string, 0, len(infos))
	for i := range infos {
		in := &infos[i]
		rows = append(rows, []string{
			in.Name,
			orDash(in.Pool),
			orDash(in.Phase),
			orDash(in.Node),
			orDash(in.Endpoint),
			formatAge(in.Age),
		})
	}
	return renderTable(header, rows)
}

const dash = "-"

func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// formatAge renders a duration as kubectl does: the single largest unit.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// renderTable lays out header + rows as a left-aligned, space-padded table with
// a trailing newline. Each column is widened to its longest cell.
func renderTable(header []string, rows [][]string) string {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}
