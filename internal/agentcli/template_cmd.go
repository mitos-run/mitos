package agentcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/yaml"

	"mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/templatebuild"
)

// cmdTemplate dispatches the `template` subcommands: build and push.
func cmdTemplate(ctx context.Context, args []string, backend TemplateBackend, out, errw io.Writer) int {
	if backend == nil {
		fmt.Fprint(errw, "template: this backend does not support templates\n")
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintf(errw, "template: a subcommand is required (build, push)\n\n%s", usage)
		return 2
	}
	switch args[0] {
	case "build":
		return cmdTemplateBuild(ctx, args[1:], backend, out, errw)
	case "push":
		return cmdTemplatePush(ctx, args[1:], backend, out, errw)
	default:
		fmt.Fprintf(errw, "unknown template subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

// cmdTemplateBuild parses a Dockerfile or a declarative spec file into a
// SandboxTemplateSpec, prints the content-addressed build plan (which steps a
// cached build would reuse), and dispatches the build. A backend build error is
// surfaced with its remediation so the failing step is named.
func cmdTemplateBuild(ctx context.Context, args []string, backend TemplateBackend, out, errw io.Writer) int {
	fs := newFlagSet("template build", errw)
	dockerfile := fs.String("dockerfile", "", "build from a Dockerfile")
	specFile := fs.String("spec", "", "build from a declarative SandboxTemplate spec (YAML or JSON)")
	name := fs.String("name", "", "template name")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(errw, usage)
		return 2
	}
	if *name == "" {
		fmt.Fprintf(errw, "template build: --name is required\n\n%s", usage)
		return 2
	}
	if *dockerfile == "" && *specFile == "" {
		fmt.Fprintf(errw, "template build: one of --dockerfile or --spec is required\n\n%s", usage)
		return 2
	}
	if *dockerfile != "" && *specFile != "" {
		fmt.Fprintf(errw, "template build: --dockerfile and --spec are mutually exclusive\n\n%s", usage)
		return 2
	}

	spec, err := loadSpec(*dockerfile, *specFile)
	if err != nil {
		fmt.Fprintf(errw, "template build: %v\n", err)
		return 1
	}

	// Print the build plan: with no cache every step is BUILD; the chained,
	// content-addressed keys are computed host-side so a real cached build can
	// reuse the unchanged prefix. The real reuse runs on the KVM node.
	plan := templatebuild.Plan(spec.Image, spec.BuildSteps, nil)
	fmt.Fprintf(out, "Building template %q from %s\n", *name, spec.Image)
	fmt.Fprint(out, templatebuild.Summary(plan))

	if err := backend.Build(ctx, *name, spec); err != nil {
		surfaceBuildError(errw, err)
		return 1
	}
	fmt.Fprintf(out, "Template %q submitted; the node builds the snapshot.\n", *name)
	return 0
}

func cmdTemplatePush(ctx context.Context, args []string, backend TemplateBackend, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintf(errw, "template push: a template name is required\n\n%s", usage)
		return 2
	}
	name := args[0]
	if err := backend.Push(ctx, name); err != nil {
		fmt.Fprintf(errw, "template push: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "pushed %s\n", name)
	return 0
}

// loadSpec reads a spec from a Dockerfile or a declarative spec file. Exactly
// one of dockerfile or specFile is set (the caller enforces this).
func loadSpec(dockerfile, specFile string) (v1alpha1.SandboxTemplateSpec, error) {
	if dockerfile != "" {
		b, err := os.ReadFile(dockerfile) //nolint:gosec // operator-supplied path
		if err != nil {
			return v1alpha1.SandboxTemplateSpec{}, fmt.Errorf("read dockerfile: %w", err)
		}
		return templatebuild.ParseDockerfile(string(b))
	}
	b, err := os.ReadFile(specFile) //nolint:gosec // operator-supplied path
	if err != nil {
		return v1alpha1.SandboxTemplateSpec{}, fmt.Errorf("read spec: %w", err)
	}
	var spec v1alpha1.SandboxTemplateSpec
	if err := yaml.Unmarshal(b, &spec); err != nil {
		return v1alpha1.SandboxTemplateSpec{}, fmt.Errorf("parse spec: %w", err)
	}
	if spec.Image == "" {
		return spec, errors.New("spec has no image")
	}
	return spec, nil
}

// surfaceBuildError prints a build error with its typed remediation. A
// templatebuild.StepError carries the failing step index, kind, and a
// remediation; a plain error is printed as-is.
func surfaceBuildError(errw io.Writer, err error) {
	var se *templatebuild.StepError
	if errors.As(err, &se) {
		env := se.APIError()
		fmt.Fprintf(errw, "template build: %s\n", se.Error())
		fmt.Fprintf(errw, "  code: %s\n", env.Code)
		fmt.Fprintf(errw, "  remediation: %s\n", env.Remediation)
		return
	}
	fmt.Fprintf(errw, "template build: %v\n", err)
}
