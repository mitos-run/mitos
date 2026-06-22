package baosecrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mitos.run/mitos/internal/saas/console"
)

// kvEmulator is a minimal in-memory OpenBao/Vault KV-v2 backend covering the
// routes the provider uses, so the provider's path construction, per-org
// scoping, and JSON handling are tested without a real server.
type kvEmulator struct {
	mu   sync.Mutex
	data map[string]entry // logical path "orgs/<org>/<name>" -> entry
	t    *testing.T
}

type entry struct {
	version     int
	fingerprint string
}

func newEmulator(t *testing.T) *httptest.Server {
	e := &kvEmulator{data: map[string]entry{}, t: t}
	return httptest.NewServer(http.HandlerFunc(e.serve))
}

func (e *kvEmulator) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r.Header.Get("X-Vault-Token") != "test-token" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v1/secret/")
	switch {
	case strings.HasPrefix(p, "data/"):
		logical := strings.TrimPrefix(p, "data/")
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			var body struct {
				Data map[string]string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			cur := e.data[logical]
			cur.version++
			e.data[logical] = cur
			writeJSON(w, map[string]any{"data": map[string]any{"version": cur.version}})
			return
		}
	case strings.HasPrefix(p, "metadata/"):
		logical := strings.TrimPrefix(p, "metadata/")
		if r.URL.Query().Get("list") == "true" || r.Method == "LIST" {
			prefix := strings.TrimSuffix(logical, "/") + "/"
			keys := []string{}
			for k := range e.data {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, strings.TrimPrefix(k, prefix))
				}
			}
			writeJSON(w, map[string]any{"data": map[string]any{"keys": keys}})
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				CustomMetadata map[string]string `json:"custom_metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ent := e.data[logical]
			ent.fingerprint = body.CustomMetadata["fingerprint"]
			e.data[logical] = ent
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			ent, ok := e.data[logical]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"data": map[string]any{
				"current_version": ent.version,
				"custom_metadata": map[string]string{"fingerprint": ent.fingerprint},
			}})
			return
		}
		if r.Method == http.MethodDelete {
			delete(e.data, logical)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newProvider(t *testing.T) *Provider {
	t.Helper()
	srv := newEmulator(t)
	t.Cleanup(srv.Close)
	return New(Config{Address: srv.URL, Token: "test-token", Mount: "secret"})
}

func TestPutStoresExternalReferenceAndVersions(t *testing.T) {
	p := newProvider(t)
	v1, err := p.Put(context.Background(), "alice", "OPENAI_API_KEY", "sk-1")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if v1.Provider != "openbao" || v1.Mode != "external_reference" || v1.Version != 1 {
		t.Fatalf("view = %+v, want openbao/external_reference/v1", v1)
	}
	if v1.Fingerprint == "" || strings.Contains(v1.Fingerprint, "sk-1") {
		t.Fatalf("fingerprint leaks or empty: %q", v1.Fingerprint)
	}
	v2, _ := p.Put(context.Background(), "alice", "OPENAI_API_KEY", "sk-2")
	if v2.Version != 2 {
		t.Fatalf("rotate version = %d, want 2", v2.Version)
	}
}

func TestListScopedToOrgPath(t *testing.T) {
	p := newProvider(t)
	_, _ = p.Put(context.Background(), "alice", "A", "x")
	_, _ = p.Put(context.Background(), "bob", "B", "y")
	alice, err := p.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(alice) != 1 || alice[0].Name != "A" {
		t.Fatalf("alice = %+v, want [A]", alice)
	}
}

func TestDeleteThenMissingIsNotFound(t *testing.T) {
	p := newProvider(t)
	_, _ = p.Put(context.Background(), "alice", "K", "x")
	if err := p.Delete(context.Background(), "alice", "K"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := p.Delete(context.Background(), "alice", "K"); err != console.ErrNotFound {
		t.Fatalf("missing delete = %v, want ErrNotFound", err)
	}
}

func TestImplementsSecretStore(t *testing.T) {
	var _ console.SecretStore = (*Provider)(nil)
}
