package agentcli

import (
	"context"
	"errors"
	"sync"

	v1 "mitos.run/mitos/api/v1"
)

// TemplateBackend is the template authoring surface the CLI dispatches to:
// build a template from a declarative spec (issue #220) and push (publish) it.
// It is narrow on purpose so tests supply a FakeTemplateBackend and the cluster
// backend applies a SandboxPool with the inline PoolTemplateSpec. The real
// snapshot build is performed by forkd on a KVM node once the pool exists; the
// CLI only authors the object and reports the build plan.
type TemplateBackend interface {
	// Build creates or updates the SandboxPool named name from spec. On the
	// cluster backend this applies the pool; the node then builds the snapshot.
	// A build-step failure surfaces as a typed error (templatebuild.StepError).
	Build(ctx context.Context, name string, spec v1.PoolTemplateSpec) error
	// Push publishes an already-built template so other nodes or environments can
	// pull it (Daytona `snapshot push` parity).
	Push(ctx context.Context, name string) error
}

// BuildCall records a build dispatch for assertions.
type BuildCall struct {
	Name string
	Spec v1.PoolTemplateSpec
}

// FakeTemplateBackend records build and push calls for CLI tests.
type FakeTemplateBackend struct {
	mu     sync.Mutex
	Builds []BuildCall
	Pushes []string

	// BuildErr, if set, is returned by Build so the error path can be exercised.
	BuildErr error
	// PushErr, if set, is returned by Push.
	PushErr error
}

// errTemplateBuildBoom is a canned build error for the error-path test.
var errTemplateBuildBoom = errors.New("template build failed at step 1 (run): exit status 1; remediation: fix the step and rebuild")

// Build implements TemplateBackend.
func (f *FakeTemplateBackend) Build(_ context.Context, name string, spec v1.PoolTemplateSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Builds = append(f.Builds, BuildCall{Name: name, Spec: spec})
	return f.BuildErr
}

// Push implements TemplateBackend.
func (f *FakeTemplateBackend) Push(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Pushes = append(f.Pushes, name)
	return f.PushErr
}
