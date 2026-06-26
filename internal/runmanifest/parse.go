package runmanifest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// dnsLabel matches a Kubernetes DNS label: lowercase alphanumerics and hyphens,
// not starting or ending with a hyphen, at most 63 characters. The name becomes a
// pool name and a URL label, so it must be a valid DNS label.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// envName matches a POSIX-ish environment variable name (the shape a secret or
// env key must take).
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse decodes a mitos.yaml manifest and validates it. It rejects unknown fields
// (strict) so a typo fails loudly with an actionable error rather than silently
// dropping config. The returned Manifest is safe to map with GoldenPool.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalStrict(data, &m); err != nil {
		return nil, fmt.Errorf("mitos.yaml: parse: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest is internally consistent and complete enough to
// provision. Errors carry the field and an actionable remediation, per the API v2
// LLM-legible error rule (#28).
func (m *Manifest) Validate() error {
	if m.Version != 0 && m.Version != SchemaVersion {
		return fmt.Errorf("mitos.yaml: unsupported version %d; this build understands version %d", m.Version, SchemaVersion)
	}
	if m.Name == "" {
		return fmt.Errorf("mitos.yaml: name is required (a DNS label, for example \"openclaw\")")
	}
	if !dnsLabel.MatchString(m.Name) {
		return fmt.Errorf("mitos.yaml: name %q must be a DNS label (lowercase alphanumerics and hyphens, no leading or trailing hyphen, max 63 chars)", m.Name)
	}

	// Source: exactly one of image or build.
	hasImage := m.Source.Image != ""
	hasBuild := m.Source.Build != nil
	switch {
	case hasImage && hasBuild:
		return fmt.Errorf("mitos.yaml: source must set exactly one of image or build, not both")
	case !hasImage && !hasBuild:
		return fmt.Errorf("mitos.yaml: source requires an image (for example ghcr.io/org/app:latest) or a build block")
	}
	if hasBuild && m.Source.Build.Repo == "" {
		return fmt.Errorf("mitos.yaml: source.build.repo is required when building from source")
	}
	if t := m.Source.Track; t != nil {
		if t.Watch == "" {
			return fmt.Errorf("mitos.yaml: source.track.watch is required (the image or repo to watch for releases)")
		}
		switch t.OnNewRelease {
		case "", ResnapshotOfferRebase, ResnapshotAutoRebase, ResnapshotOnly:
		default:
			return fmt.Errorf("mitos.yaml: source.track.on_new_release %q is not one of resnapshot+offer-rebase, resnapshot+auto-rebase, resnapshot", t.OnNewRelease)
		}
	}

	// Run.
	for k := range m.Run.Env {
		if !envName.MatchString(k) {
			return fmt.Errorf("mitos.yaml: run.env key %q is not a valid environment variable name", k)
		}
	}
	if r := m.Run.Ready; r != nil {
		if r.HTTP == nil {
			return fmt.Errorf("mitos.yaml: run.ready needs an http gate (run.ready.http.port)")
		}
		if r.HTTP.Port <= 0 || r.HTTP.Port > 65535 {
			return fmt.Errorf("mitos.yaml: run.ready.http.port %d is out of range 1-65535", r.HTTP.Port)
		}
		if r.Timeout != "" {
			if _, err := time.ParseDuration(r.Timeout); err != nil {
				return fmt.Errorf("mitos.yaml: run.ready.timeout %q is not a duration (for example 90s): %w", r.Timeout, err)
			}
		}
	}

	// Preview.
	if m.Preview.Port <= 0 || m.Preview.Port > 65535 {
		return fmt.Errorf("mitos.yaml: preview.port is required and must be 1-65535 (the port your live surface serves on)")
	}
	switch m.Preview.Auth {
	case "", "ladder", "required":
	default:
		return fmt.Errorf("mitos.yaml: preview.auth %q is not one of ladder, required", m.Preview.Auth)
	}

	// Secrets.
	seen := map[string]bool{}
	for i, s := range m.Secrets {
		if s.Name == "" {
			return fmt.Errorf("mitos.yaml: secrets[%d].name is required", i)
		}
		if !envName.MatchString(s.Name) {
			return fmt.Errorf("mitos.yaml: secrets[%d].name %q is not a valid environment variable name", i, s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("mitos.yaml: secret %q is declared more than once", s.Name)
		}
		seen[s.Name] = true
		if s.Generate < 0 {
			return fmt.Errorf("mitos.yaml: secrets[%d].generate must be >= 0", i)
		}
	}

	// Egress.
	for i, a := range m.Egress.Allow {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("mitos.yaml: egress.allow[%d] is empty", i)
		}
	}

	// Workspace.
	if m.Workspace != nil && m.Workspace.Path == "" {
		return fmt.Errorf("mitos.yaml: workspace.path is required when a workspace is declared")
	}

	// Resources.
	if m.Resources.CPU != "" {
		if _, err := resource.ParseQuantity(m.Resources.CPU); err != nil {
			return fmt.Errorf("mitos.yaml: resources.cpu %q is not a quantity (for example \"2\"): %w", m.Resources.CPU, err)
		}
	}
	if m.Resources.Memory != "" {
		if _, err := resource.ParseQuantity(m.Resources.Memory); err != nil {
			return fmt.Errorf("mitos.yaml: resources.memory %q is not a quantity (for example \"2Gi\"): %w", m.Resources.Memory, err)
		}
	}
	switch m.Resources.Lifetime {
	case "", "persistent", "ephemeral":
	default:
		return fmt.Errorf("mitos.yaml: resources.lifetime %q is not one of persistent, ephemeral", m.Resources.Lifetime)
	}
	return nil
}
