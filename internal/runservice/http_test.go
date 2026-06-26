package runservice

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeIdentity struct {
	id  Identity
	err error
}

func (f fakeIdentity) Resolve(*http.Request, string) (Identity, error) {
	return f.id, f.err
}

func newTestServer(t *testing.T, idr IdentityResolver) *httptest.Server {
	t.Helper()
	svc := New(&fakeFetcher{m: mustManifest(t, openclawYAML)}, &fakeApplier{}, "mitos.run")
	mux := http.NewServeMux()
	NewHandler(svc, idr).Routes(mux)
	return httptest.NewServer(mux)
}

func TestDescribeHandler(t *testing.T) {
	srv := newTestServer(t, fakeIdentity{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/run/describe?src=github.com/openclaw/openclaw")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestDescribeHandlerMissingSrc(t *testing.T) {
	srv := newTestServer(t, fakeIdentity{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/run/describe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRunHandler(t *testing.T) {
	idr := fakeIdentity{id: Identity{Namespace: "tenant-jannes", InstanceLabel: "jannes-openclaw"}}
	srv := newTestServer(t, idr)
	defer srv.Close()
	body := `{"src":"github.com/openclaw/openclaw","secrets":{"ANTHROPIC_API_KEY":"sk-real"}}`
	resp, err := http.Post(srv.URL+"/run", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestRunHandlerUnauthorized(t *testing.T) {
	srv := newTestServer(t, fakeIdentity{err: errors.New("no session")})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/run", "application/json",
		strings.NewReader(`{"src":"github.com/openclaw/openclaw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRunHandlerMissingRequiredSecret(t *testing.T) {
	idr := fakeIdentity{id: Identity{Namespace: "ns", InstanceLabel: "inst"}}
	srv := newTestServer(t, idr)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/run", "application/json",
		strings.NewReader(`{"src":"github.com/openclaw/openclaw"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}
