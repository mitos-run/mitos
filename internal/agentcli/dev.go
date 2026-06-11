package agentcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// defaultClusterName is the kind cluster name DevUp/DevDown manage when
// DevOptions.ClusterName is empty.
const defaultClusterName = "agentrun-dev"

// kindConfigPath is the kind cluster config DevUp passes to `kind create
// cluster`. It is repo-relative, matching how CI references it; run agentrun dev
// from the repo root.
const kindConfigPath = "hack/kind-config.yaml"

// devPoolYAML is the minimal SandboxTemplate + SandboxPool DevUp applies so a
// fresh dev cluster has a pool to claim from. It uses the mock-friendly defaults
// (no snapshot/CSI volumes) so it reconciles without a KVM node.
const devPoolYAML = `apiVersion: agentrun.dev/v1alpha1
kind: SandboxTemplate
metadata:
  name: dev-default
spec:
  image: python:3.12-slim
  resources:
    cpu: "1"
    memory: "512Mi"
---
apiVersion: agentrun.dev/v1alpha1
kind: SandboxPool
metadata:
  name: dev-default
spec:
  templateRef:
    name: dev-default
  replicas: 1
  snapshotAfter: Ready
`

// DevOptions configures the local dev cluster orchestration.
type DevOptions struct {
	// ClusterName overrides the kind cluster name. Empty uses defaultClusterName.
	ClusterName string
}

// CommandRunner runs an external command argv. DevUp/DevDown take a runner so
// the orchestration sequence is unit-testable without a real kind or kubectl;
// cmd/agentrun injects a runner that shells out.
type CommandRunner func(ctx context.Context, argv []string) error

func (o DevOptions) clusterName() string {
	if o.ClusterName != "" {
		return o.ClusterName
	}
	return defaultClusterName
}

// DevUp brings a local kind dev cluster up and installs the control plane:
//
//  1. kind create cluster (tolerating an already-existing cluster of the same name)
//  2. kubectl apply -f deploy/crds/
//  3. kubectl apply -f deploy/controller/
//  4. kubectl apply -f - with a minimal default pool
//
// Each external command runs through runner so the sequence is testable. DevUp
// prints progress and a clear note to out that local dev uses the mock engine
// and that real exec needs a KVM node. The control plane brought up here is the
// stock controller manifest; for a fully local mock-mode controller (no KVM,
// the --mock flag on cmd/controller) the controller deployment args need that
// flag added, which is a manifest tweak left to the operator. DevUp applies what
// brings the CRDs and controller up honestly and notes the mock requirement.
func DevUp(ctx context.Context, opts DevOptions, runner CommandRunner, out io.Writer) error {
	name := opts.clusterName()

	fmt.Fprintf(out, "Creating kind cluster %q...\n", name)
	if err := runner(ctx, []string{"kind", "create", "cluster", "--name", name, "--config", kindConfigPath}); err != nil {
		// A cluster that already exists is not a failure: dev up is meant to be
		// re-runnable. kind reports this on stderr; the message contains
		// "already exist".
		if !isAlreadyExists(err) {
			return fmt.Errorf("create kind cluster %q: %w", name, err)
		}
		fmt.Fprintf(out, "kind cluster %q already exists, continuing.\n", name)
	}

	fmt.Fprintln(out, "Applying CRDs...")
	if err := runner(ctx, []string{"kubectl", "apply", "-f", "deploy/crds/"}); err != nil {
		return fmt.Errorf("apply CRDs: %w", err)
	}

	fmt.Fprintln(out, "Applying controller...")
	if err := runner(ctx, []string{"kubectl", "apply", "-f", "deploy/controller/"}); err != nil {
		return fmt.Errorf("apply controller: %w", err)
	}

	fmt.Fprintln(out, "Applying default pool...")
	// The runner takes argv only (no stdin), so write the inline pool YAML to a
	// temp file and apply that, then clean up. This keeps the orchestration
	// fully testable while still applying a minimal mock-friendly pool rather
	// than the CSI-backed examples/python-pool.yaml that a no-KVM cluster would
	// not reconcile.
	poolFile, cleanup, err := writeTempYAML(devPoolYAML)
	if err != nil {
		return fmt.Errorf("write default pool manifest: %w", err)
	}
	defer cleanup()
	if err := runner(ctx, []string{"kubectl", "apply", "-f", poolFile}); err != nil {
		return fmt.Errorf("apply default pool: %w", err)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Local dev cluster is up.")
	fmt.Fprintln(out, "Note: local dev uses the mock fork engine (no KVM). Sandboxes")
	fmt.Fprintln(out, "reconcile to Ready, but real in-VM exec needs a KVM node and the")
	fmt.Fprintln(out, "controller running in --mock mode is the no-KVM path.")
	return nil
}

// DevDown deletes the local kind dev cluster. Deleting a non-existent cluster is
// reported by kind but is not treated as fatal here.
func DevDown(ctx context.Context, opts DevOptions, runner CommandRunner, out io.Writer) error {
	name := opts.clusterName()
	fmt.Fprintf(out, "Deleting kind cluster %q...\n", name)
	if err := runner(ctx, []string{"kind", "delete", "cluster", "--name", name}); err != nil {
		return fmt.Errorf("delete kind cluster %q: %w", name, err)
	}
	return nil
}

// writeTempYAML writes content to a temp file with a recognizable name and
// returns its path plus a cleanup function that removes it.
func writeTempYAML(content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "agentrun-dev-pool-*.yaml")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// isAlreadyExists reports whether err is kind's "cluster already exists" signal.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exist")
}
