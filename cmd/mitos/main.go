// Command mitos is the command-line interface for snapshot-fork sandboxes.
// It drives the sandbox lifecycle (create, exec, file IO, fork, terminate, list)
// against a Kubernetes cluster, and brings a local kind dev cluster up or down.
//
// Usage:
//
//	mitos run <command> [--pool P] [--timeout N]
//	mitos sandbox create|ls|exec|fork|terminate ...
//	mitos dev up|down
//
// The cluster connection is resolved from the standard kubeconfig (KUBECONFIG,
// --kubeconfig, or in-cluster). The sandbox API bearer token is read from the
// per-sandbox Secret at request time and is never logged.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"mitos.run/mitos/internal/agentcli"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses the global flags, splits the dev path (which needs no cluster
// backend) from the sandbox path, and dispatches into agentcli.Run.
func run(args []string) int {
	ctx := context.Background()

	// Global flags may precede the subcommand: --namespace and --pool. Parse
	// them out manually so they can appear before the subcommand without the
	// stdlib flag parser swallowing the subcommand's own flags.
	namespace, pool, rest := parseGlobalFlags(args)

	if len(rest) == 0 {
		return agentcli.Run(ctx, rest, nil, os.Stdout, os.Stderr)
	}

	// version prints build metadata (injected via -ldflags at release time) and
	// needs no cluster backend, so it is handled before any kubeconfig probe.
	if rest[0] == "version" || rest[0] == "--version" || rest[0] == "-v" {
		printVersion(os.Stdout)
		return 0
	}

	// The dev subcommand orchestrates kind/kubectl and needs no cluster backend.
	if rest[0] == "dev" {
		return runDev(ctx, rest[1:])
	}

	// The doctor subcommand reads node state (/dev/kvm, /proc/modules, the staged
	// guest kernel) and, when a kubeconfig is reachable, cluster state (PKI
	// secrets, pull secret, PSA label). The node checks must run even without a
	// cluster, so build a best-effort client and proceed regardless.
	if rest[0] == "doctor" {
		return runDoctorCmd(ctx, namespace, rest[1:])
	}

	// Usage is printable without a cluster, so a developer with no kubeconfig
	// can still discover the commands.
	if rest[0] == "-h" || rest[0] == "--help" || rest[0] == "help" {
		return agentcli.Run(ctx, rest, nil, os.Stdout, os.Stderr)
	}

	// The auth subcommands talk to the hosted account service, not the cluster,
	// and must work without a kubeconfig. Wire a local account service and
	// dispatch directly so `mitos auth login` does not require a cluster backend.
	if rest[0] == "auth" {
		return agentcli.Run(ctx, rest, withLocalAuth(nil), os.Stdout, os.Stderr)
	}

	backend, err := buildBackend(namespace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	// When a global --pool is set and the subcommand is one that creates a
	// sandbox without its own --pool, default it in by appending the flag. The
	// CLI's own --pool on the subcommand still wins because flag parsing takes
	// the last value.
	rest = applyDefaultPool(rest, pool)

	return agentcli.Run(ctx, rest, backend, os.Stdout, os.Stderr)
}

// parseGlobalFlags extracts a leading --namespace/-n and --pool from args,
// returning them plus the remaining args (the subcommand and its arguments).
// Only flags that appear before the first non-flag token are consumed.
func parseGlobalFlags(args []string) (namespace, pool string, rest []string) {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--namespace", "-n":
			if i+1 < len(args) {
				namespace = args[i+1]
				i += 2
				continue
			}
			i++
		case "--pool":
			if i+1 < len(args) {
				pool = args[i+1]
				i += 2
				continue
			}
			i++
		default:
			return namespace, pool, args[i:]
		}
	}
	return namespace, pool, args[i:]
}

// applyDefaultPool injects a --pool flag for the create-style subcommands when a
// global pool was given and the subcommand did not specify its own. It is a
// best-effort convenience; the subcommand's explicit --pool always wins.
func applyDefaultPool(rest []string, pool string) []string {
	if pool == "" {
		return rest
	}
	hasPool := false
	for _, a := range rest {
		if a == "--pool" {
			hasPool = true
			break
		}
	}
	if hasPool {
		return rest
	}
	switch {
	case len(rest) >= 1 && rest[0] == "run":
		out := append([]string{rest[0], "--pool", pool}, rest[1:]...)
		return out
	case len(rest) >= 2 && rest[0] == "sandbox" && rest[1] == "create":
		out := append([]string{rest[0], rest[1], "--pool", pool}, rest[2:]...)
		return out
	default:
		return rest
	}
}

// runDoctorCmd runs the `mitos doctor` preflight. It builds a best-effort
// cluster client: if no kubeconfig is reachable the cluster checks report a
// probe error (surfaced as failing checks) but the node checks still run, so an
// operator on a KVM node without cluster access still gets the node verdict. A
// -n/--namespace flag (also accepted as a global flag) selects the install
// namespace.
func runDoctorCmd(ctx context.Context, namespace string, args []string) int {
	namespace = doctorNamespace(namespace, args)
	cfg := agentcli.DoctorProbeConfig{Namespace: namespace}
	if rc, err := ctrlconfig.GetConfig(); err == nil {
		if c, cerr := client.New(rc, client.Options{Scheme: agentcli.Scheme()}); cerr == nil {
			cfg.Client = c
		} else {
			fmt.Fprintln(os.Stderr, "doctor: cluster checks skipped: build client:", cerr)
		}
	} else {
		fmt.Fprintln(os.Stderr, "doctor: cluster checks skipped: no reachable kubeconfig; node checks still run")
	}
	probe := agentcli.NewRealProbe(cfg)
	return agentcli.Doctor(ctx, probe, os.Stdout, os.Stderr)
}

// doctorNamespace resolves the install namespace for `mitos doctor`. A local
// -n/--namespace flag wins over the global one; if neither is set it defaults to
// "mitos".
func doctorNamespace(global string, args []string) string {
	ns := global
	for i := 0; i < len(args); i++ {
		if (args[i] == "-n" || args[i] == "--namespace") && i+1 < len(args) {
			ns = args[i+1]
			i++
		}
	}
	if ns == "" {
		ns = "mitos"
	}
	return ns
}

// buildBackend resolves the kubeconfig and builds a cluster backend scoped to
// namespace (empty means the backend default).
func buildBackend(namespace string) (*agentcli.ClusterBackend, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: agentcli.Scheme()})
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return agentcli.NewClusterBackend(c, namespace, nil), nil
}

// runDev dispatches the dev subcommand (up|down) using a runner that shells out
// to kind and kubectl. Output goes to stdout; errors to stderr.
func runDev(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "dev: 'up' or 'down' is required")
		return 2
	}
	runner := func(ctx context.Context, argv []string) error {
		if len(argv) == 0 {
			return fmt.Errorf("empty command")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	switch args[0] {
	case "up":
		// --skip-cluster-create targets an already-running cluster (the current
		// kubectl context) instead of running `kind create`; CI uses it to apply
		// the dev control plane onto a cluster it stood up itself.
		opts := agentcli.DevOptions{}
		for _, a := range args[1:] {
			if a == "--skip-cluster-create" {
				opts.SkipClusterCreate = true
			}
		}
		if err := agentcli.DevUp(ctx, opts, runner, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "dev up:", err)
			return 1
		}
		return 0
	case "down":
		if err := agentcli.DevDown(ctx, agentcli.DevOptions{}, runner, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "dev down:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown dev subcommand %q\n", args[0])
		return 2
	}
}
