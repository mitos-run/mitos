// Package runservice turns a "Run with Mitos" click into a provisioned instance:
// it fetches a repo's mitos.yaml, ensures the golden SandboxPool, provisions the
// per-fork Sandbox and its Secret, applies them, and returns the live URL.
//
// It is the seam between the front door (the run HTTP endpoint, the badge) and the
// cluster. Dependencies (manifest fetch, object apply, tenant identity) are
// injected so the core is unit-testable without a cluster or network. Secret
// VALUES flow through Run into the applied Secret only; they are never logged or
// returned.
//
// Tenancy: the golden pool lands in the caller's tenant namespace (isolation-first,
// per the per-org namespace model #410), so a fork shares pages copy-on-write with
// the tenant's own golden. A cross-tenant shared-golden cost tier is a deliberate
// later decision (it trades namespace isolation for wider CoW sharing) and is out
// of scope here.
package runservice

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/internal/runmanifest"
)

// ManifestFetcher fetches and parses a repo's mitos.yaml for a source reference
// such as "github.com/openclaw/openclaw".
type ManifestFetcher interface {
	Fetch(ctx context.Context, src string) (*runmanifest.Manifest, error)
}

// Applier upserts cluster objects (the golden pool, the per-fork Secret, the
// Sandbox). Implementations make it idempotent so re-running an app reuses the
// existing golden pool rather than failing on conflict.
type Applier interface {
	Apply(ctx context.Context, objs ...client.Object) error
}

// Identity is the resolved tenant for one click: the namespace the instance lands
// in and the unique DNS label that becomes its subdomain. The real resolver maps
// the signed-in user to their org namespace (#410) and a per-user-per-app label.
type Identity struct {
	Namespace     string
	InstanceLabel string
}

// RunDescription is the consent contract shown before provisioning: what forks,
// what it needs, where it may egress. It carries NO secret values.
type RunDescription struct {
	Name    string         `json:"name"`
	Title   string         `json:"title,omitempty"`
	Image   string         `json:"image,omitempty"`
	Secrets []SecretPrompt `json:"secrets,omitempty"`
	Egress  []string       `json:"egress,omitempty"`
}

// SecretPrompt is one secret the clicker is asked for.
type SecretPrompt struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required"`
	Generate bool   `json:"generate"`
}

// RunResult is the outcome of a successful run.
type RunResult struct {
	Instance string `json:"instance"`
	URL      string `json:"url"`
}

// Service provisions instances from manifests.
type Service struct {
	fetch        ManifestFetcher
	apply        Applier
	exposeDomain string
}

// New builds a Service. exposeDomain is the cluster's expose domain (for example
// "mitos.run"); the live URL is "<instanceLabel>.<exposeDomain>".
func New(f ManifestFetcher, a Applier, exposeDomain string) *Service {
	return &Service{fetch: f, apply: a, exposeDomain: exposeDomain}
}

// Describe fetches the manifest and returns the consent contract for src.
func (s *Service) Describe(ctx context.Context, src string) (*RunDescription, error) {
	m, err := s.fetch.Fetch(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("describe %q: %w", src, err)
	}
	d := &RunDescription{Name: m.Name, Title: m.Title, Image: m.Source.Image, Egress: m.Egress.Allow}
	for _, sec := range m.Secrets {
		d.Secrets = append(d.Secrets, SecretPrompt{
			Name:     sec.Name,
			Label:    sec.Label,
			Required: sec.Required,
			Generate: sec.Generate > 0,
		})
	}
	return d, nil
}

// Run provisions an instance of src for the given identity and supplied secrets:
// ensure the golden pool, provision the per-fork objects, apply them, and return
// the live URL. The golden pool, the Secret, and the Sandbox are applied together;
// the Applier upserts so a repeat run reuses the existing golden.
func (s *Service) Run(ctx context.Context, src string, id Identity, secrets map[string]string) (*RunResult, error) {
	if id.Namespace == "" || id.InstanceLabel == "" {
		return nil, fmt.Errorf("run: identity needs a namespace and an instance label")
	}
	m, err := s.fetch.Fetch(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("run %q: %w", src, err)
	}
	pool, err := m.GoldenPool(id.Namespace)
	if err != nil {
		return nil, fmt.Errorf("run %q: golden pool: %w", src, err)
	}
	res, err := runmanifest.Provision(m, secrets, id.Namespace, id.InstanceLabel)
	if err != nil {
		return nil, fmt.Errorf("run %q: provision: %w", src, err)
	}
	if err := s.apply.Apply(ctx, pool, res.Secret, res.Sandbox); err != nil {
		return nil, fmt.Errorf("run %q: apply: %w", src, err)
	}
	return &RunResult{
		Instance: id.InstanceLabel,
		URL:      fmt.Sprintf("https://%s.%s", id.InstanceLabel, s.exposeDomain),
	}, nil
}
