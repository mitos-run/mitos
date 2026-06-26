package runservice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManifestURL(t *testing.T) {
	const base = "https://raw.githubusercontent.com"
	cases := []struct {
		src  string
		want string
		ok   bool
	}{
		{"github.com/openclaw/openclaw", base + "/openclaw/openclaw/HEAD/mitos.yaml", true},
		{"https://github.com/bytedance/deer-flow.git", base + "/bytedance/deer-flow/HEAD/mitos.yaml", true},
		{"openclaw/openclaw", base + "/openclaw/openclaw/HEAD/mitos.yaml", true},
		{"github.com/onlyowner", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := manifestURL(base, c.src)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("manifestURL(%q) = %q, %v; want %q", c.src, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("manifestURL(%q) should error", c.src)
		}
	}
}

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/openclaw/openclaw/HEAD/mitos.yaml") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(openclawYAML))
	}))
	defer srv.Close()

	g := &GitHubFetcher{baseURL: srv.URL}
	m, err := g.Fetch(context.Background(), "github.com/openclaw/openclaw")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Name != "openclaw" {
		t.Errorf("manifest name = %q", m.Name)
	}
}

func TestFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	g := &GitHubFetcher{baseURL: srv.URL}
	_, err := g.Fetch(context.Background(), "github.com/no/such")
	if err == nil || !strings.Contains(err.Error(), "no mitos.yaml") {
		t.Fatalf("want not-found error, got %v", err)
	}
}
