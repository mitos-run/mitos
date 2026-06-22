// Package baosecrets is the OpenBao (and Vault) SecretStore provider: the
// recommended external backend behind the console.SecretStore seam (spec §8).
// OpenBao is the Linux-Foundation open-source fork of Vault; the same KV-v2 HTTP
// API serves both, so this one provider covers either.
//
// Per-tenant isolation (path mode): every org's secrets live under the path
// prefix orgs/<orgID>/ in the configured KV-v2 mount, and the operator scopes a
// per-org policy/AppRole to that prefix. The console BFF additionally enforces
// org scope on top, so a session for org A can never reference org B's path.
//
// Secrets are external_reference from mitos's perspective: the value lives in
// OpenBao, and the console returns only metadata (version + a non-sensitive
// fingerprint stored as custom_metadata); the value is never read back through
// the BFF.
package baosecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"mitos.run/mitos/internal/saas/console"
)

// Config wires the provider to an OpenBao/Vault server.
type Config struct {
	Address string       // e.g. https://bao.example.com
	Token   string       // X-Vault-Token; in production a per-org AppRole token
	Mount   string       // KV-v2 mount, e.g. "secret"
	HTTP    *http.Client // optional; defaults to http.DefaultClient
}

// Provider implements console.SecretStore against an OpenBao/Vault KV-v2 mount.
type Provider struct {
	addr  string
	token string
	mount string
	http  *http.Client
}

// New builds an OpenBao provider.
func New(cfg Config) *Provider {
	c := cfg.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	mount := cfg.Mount
	if mount == "" {
		mount = "secret"
	}
	return &Provider{addr: cfg.Address, token: cfg.Token, mount: mount, http: c}
}

func (p *Provider) orgPrefix(orgID string) string { return "orgs/" + orgID }

// Put writes the value to OpenBao and records a fingerprint as custom_metadata.
func (p *Provider) Put(ctx context.Context, orgID, name, value string) (console.SecretView, error) {
	logical := p.orgPrefix(orgID) + "/" + name
	fp := console.Fingerprint(value)

	var dataResp struct {
		Data struct {
			Version int `json:"version"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodPut, "data/"+logical,
		map[string]any{"data": map[string]string{"value": value}}, &dataResp); err != nil {
		return console.SecretView{}, err
	}
	// Best-effort custom_metadata for the fingerprint; the value write is the
	// load-bearing call.
	_ = p.do(ctx, http.MethodPost, "metadata/"+logical,
		map[string]any{"custom_metadata": map[string]string{"fingerprint": fp}}, nil)

	return console.SecretView{Name: name, OrgID: orgID, Provider: "openbao", Mode: "external_reference", Version: dataResp.Data.Version, Fingerprint: fp}, nil
}

// List returns the org's secrets (metadata only) under its path prefix.
func (p *Provider) List(ctx context.Context, orgID string) ([]console.SecretView, error) {
	var listResp struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := p.do(ctx, http.MethodGet, "metadata/"+p.orgPrefix(orgID)+"?list=true", nil, &listResp); err != nil {
		return nil, err
	}
	out := make([]console.SecretView, 0, len(listResp.Data.Keys))
	for _, name := range listResp.Data.Keys {
		var md struct {
			Data struct {
				CurrentVersion int               `json:"current_version"`
				CustomMetadata map[string]string `json:"custom_metadata"`
			} `json:"data"`
		}
		if err := p.do(ctx, http.MethodGet, "metadata/"+p.orgPrefix(orgID)+"/"+name, nil, &md); err != nil {
			return nil, err
		}
		out = append(out, console.SecretView{
			Name: name, OrgID: orgID, Provider: "openbao", Mode: "external_reference",
			Version: md.Data.CurrentVersion, Fingerprint: md.Data.CustomMetadata["fingerprint"],
		})
	}
	return out, nil
}

// Delete removes the org's secret (all versions, via the metadata endpoint). A
// secret that does not exist is console.ErrNotFound.
func (p *Provider) Delete(ctx context.Context, orgID, name string) error {
	logical := p.orgPrefix(orgID) + "/" + name
	if err := p.do(ctx, http.MethodGet, "metadata/"+logical, nil, nil); err != nil {
		return err // notFound maps to console.ErrNotFound in do()
	}
	return p.do(ctx, http.MethodDelete, "metadata/"+logical, nil, nil)
}

// do performs one KV-v2 request. A 404 is mapped to console.ErrNotFound; other
// non-2xx are surfaced as errors. out, if non-nil, is JSON-decoded.
func (p *Provider) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.addr+"/v1/"+p.mount+"/"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("openbao %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return console.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openbao %s %s: status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
