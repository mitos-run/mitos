// Package compose defines the Harbor compose provider contract for a mitos
// sandbox (issue #491, part of the compose epic #487).
//
// Harbor models a multi-service evaluation environment as a docker-compose
// stack: one main service the agent execs into, plus sidecars (databases, mock
// servers, MCP servers). Harbor's provider surface advertises an
// EnvironmentCapabilities.docker_compose flag and per-service operations
// (service exec, file/dir download, is-dir, stop) plus collect hooks that gather
// sidecar artifacts at teardown (for example a pg_dump from a database sidecar
// before grading).
//
// This package is the host-side CONTRACT and ROUTER for those operations:
//
//   - typed per-service request types,
//   - input validation (service names and container paths, traversal rejected
//     the same way the sandbox file APIs reject it),
//   - a Provider that validates every request and dispatches it to a Backend,
//   - collect-hook orchestration that gathers every hook's output and never
//     aborts the batch on a single failure,
//   - an honest capability advertisement (docker_compose is true only when a
//     working backend is wired).
//
// The ACTUAL compose execution lives behind the Backend interface. The real
// backend is the in-guest privileged dockerd + docker compose, which is a
// separate, hardware/kernel-gated follow-up (issues #489 and #490) and does NOT
// exist yet. Until it is wired, UnavailableBackend is the default: it advertises
// docker_compose=false and fails closed with ErrBackendUnavailable on every
// operation. The contract, routing, validation, and collect orchestration in
// this package are therefore unit-tested against a mock backend; nothing here
// claims that compose runs end to end.
package compose

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrBackendUnavailable is returned by every per-service operation when no
// working compose backend is wired into the sandbox. It carries actionable
// remediation per the LLM-legible error rule.
var ErrBackendUnavailable = errors.New(
	"compose backend unavailable: in-guest docker compose is not running in this sandbox. " +
		"Enable the in-guest privileged dockerd plus docker compose backend (compose epic A1, issues #489 and #490) " +
		"before driving per-service compose operations",
)

// serviceNamePattern constrains a compose service name. Compose service names
// are short identifiers; this pattern forbids dots as the first character,
// slashes, whitespace, and `..` segments, so a validated name can never escape
// into a host or container path or be mistaken for a path element.
var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// Capabilities mirrors the subset of Harbor's EnvironmentCapabilities that this
// contract governs. DockerCompose is advertised true only when a working
// backend is wired, so the flag is never claimed dishonestly.
type Capabilities struct {
	DockerCompose bool
}

// ServiceExecRequest addresses an exec at a named compose service's container.
type ServiceExecRequest struct {
	// Service is the compose service name (for example "db" or "mock-server").
	Service string
	// Command is the argv to run inside the service container. Must be non-empty.
	Command []string
	// WorkDir, when set, is an absolute path inside the container.
	WorkDir string
}

// ServiceStopRequest stops a named compose service.
type ServiceStopRequest struct {
	// Service is the compose service name to stop.
	Service string
	// TimeoutSeconds is the optional graceful-stop timeout passed to the backend.
	TimeoutSeconds int
}

// ExecResult is the outcome of a per-service exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// CollectHook describes one artifact-collection step to run against a sidecar at
// teardown (for example a pg_dump before grading).
type CollectHook struct {
	// Name labels the collected artifact in the results.
	Name string
	// Service is the compose service the hook runs against.
	Service string
	// Command is the argv to run; its stdout is the collected artifact.
	Command []string
	// WorkDir, when set, is an absolute path inside the container.
	WorkDir string
}

// CollectResult is one CollectHook's outcome. Err is non-nil when that hook
// failed validation or its backend exec failed; the rest of the batch still
// runs (collect-on-teardown gathers everything it can).
type CollectResult struct {
	Name     string
	Service  string
	ExitCode int
	Output   []byte
	Err      error
}

// Backend is the per-service compose execution surface. The real implementation
// is the in-guest privileged dockerd plus docker compose (issues #489 and #490);
// it does not exist yet, so UnavailableBackend is the wired default and the mock
// backend stands in for the contract's unit tests.
type Backend interface {
	// Available reports whether a working compose runtime is present. It gates
	// the docker_compose capability so the flag is advertised honestly.
	Available() bool
	ServiceExec(ctx context.Context, req ServiceExecRequest) (ExecResult, error)
	ServiceDownloadFile(ctx context.Context, service, path string) ([]byte, error)
	ServiceDownloadDir(ctx context.Context, service, path string, exclusions []string) ([]byte, error)
	ServiceIsDir(ctx context.Context, service, path string) (bool, error)
	StopService(ctx context.Context, req ServiceStopRequest) error
}

// Provider validates inputs and routes per-service operations to a Backend. It
// is the contract Harbor (or any external compose driver) builds against.
type Provider struct {
	backend Backend
}

// NewProvider wraps a Backend. A nil backend behaves as no compose support:
// Capabilities reports docker_compose=false and operations fail closed.
func NewProvider(b Backend) *Provider {
	return &Provider{backend: b}
}

// Capabilities advertises docker_compose only when a working backend is wired.
func (p *Provider) Capabilities() Capabilities {
	return Capabilities{DockerCompose: p.backend != nil && p.backend.Available()}
}

func (p *Provider) backendOrUnavailable() Backend {
	if p.backend == nil {
		return UnavailableBackend{}
	}
	return p.backend
}

// ServiceExec validates the request and runs it against the addressed service.
func (p *Provider) ServiceExec(ctx context.Context, req ServiceExecRequest) (ExecResult, error) {
	if err := validateServiceName(req.Service); err != nil {
		return ExecResult{}, err
	}
	if len(req.Command) == 0 {
		return ExecResult{}, fmt.Errorf("invalid exec for service %q: command must be a non-empty argv", req.Service)
	}
	if req.WorkDir != "" {
		if err := validateContainerPath(req.WorkDir); err != nil {
			return ExecResult{}, fmt.Errorf("invalid workdir: %w", err)
		}
	}
	return p.backendOrUnavailable().ServiceExec(ctx, req)
}

// ServiceDownloadFile validates inputs and downloads a file from a service.
func (p *Provider) ServiceDownloadFile(ctx context.Context, service, path string) ([]byte, error) {
	if err := validateServiceName(service); err != nil {
		return nil, err
	}
	if err := validateContainerPath(path); err != nil {
		return nil, err
	}
	return p.backendOrUnavailable().ServiceDownloadFile(ctx, service, path)
}

// ServiceDownloadDir validates inputs and downloads a directory (as a stream)
// from a service, honoring the exclusion patterns.
func (p *Provider) ServiceDownloadDir(ctx context.Context, service, path string, exclusions []string) ([]byte, error) {
	if err := validateServiceName(service); err != nil {
		return nil, err
	}
	if err := validateContainerPath(path); err != nil {
		return nil, err
	}
	return p.backendOrUnavailable().ServiceDownloadDir(ctx, service, path, exclusions)
}

// ServiceIsDir validates inputs and reports whether path is a directory in a
// service container.
func (p *Provider) ServiceIsDir(ctx context.Context, service, path string) (bool, error) {
	if err := validateServiceName(service); err != nil {
		return false, err
	}
	if err := validateContainerPath(path); err != nil {
		return false, err
	}
	return p.backendOrUnavailable().ServiceIsDir(ctx, service, path)
}

// StopService validates the request and stops the addressed service.
func (p *Provider) StopService(ctx context.Context, req ServiceStopRequest) error {
	if err := validateServiceName(req.Service); err != nil {
		return err
	}
	return p.backendOrUnavailable().StopService(ctx, req)
}

// Collect runs every hook in order and returns one CollectResult per hook. A
// hook that fails validation or whose backend exec errors records its error in
// its result; the remaining hooks still run, so a single bad sidecar never loses
// the rest of the teardown artifacts.
func (p *Provider) Collect(ctx context.Context, hooks []CollectHook) []CollectResult {
	results := make([]CollectResult, 0, len(hooks))
	for _, h := range hooks {
		res := CollectResult{Name: h.Name, Service: h.Service}
		out, err := p.ServiceExec(ctx, ServiceExecRequest{
			Service: h.Service,
			Command: h.Command,
			WorkDir: h.WorkDir,
		})
		if err != nil {
			res.Err = fmt.Errorf("collect hook %q on service %q: %w", h.Name, h.Service, err)
		} else {
			res.ExitCode = out.ExitCode
			res.Output = out.Stdout
		}
		results = append(results, res)
	}
	return results
}

// validateServiceName rejects a compose service name that could escape into a
// path or is otherwise malformed (empty, traversal, slashes, whitespace).
func validateServiceName(s string) error {
	if !serviceNamePattern.MatchString(s) {
		return fmt.Errorf("invalid compose service name %q: names must be 1-63 characters of [a-zA-Z0-9_.-] starting with a letter or digit (no slashes, no dots leading, no '..', no whitespace)", s)
	}
	return nil
}

// validateContainerPath requires an absolute container path with no `..`
// segment, mirroring the traversal defense on the sandbox file APIs.
func validateContainerPath(p string) error {
	if p == "" {
		return errors.New("path must not be empty")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be absolute (start with '/')", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q must not contain a '..' segment", p)
		}
	}
	return nil
}
