package runservice

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"mitos.run/mitos/internal/runmanifest"
)

// maxManifestBytes caps a fetched manifest so a hostile or runaway source cannot
// stream unbounded data into the parser.
const maxManifestBytes = 1 << 20 // 1 MiB

// GitHubFetcher fetches mitos.yaml from a GitHub repo's default branch via
// raw.githubusercontent.com.
type GitHubFetcher struct {
	// Client is the HTTP client; nil uses http.DefaultClient.
	Client *http.Client
	// baseURL overrides the raw host (tests point it at an httptest server).
	baseURL string
}

func (g *GitHubFetcher) base() string {
	if g.baseURL != "" {
		return g.baseURL
	}
	return "https://raw.githubusercontent.com"
}

// manifestURL builds the raw mitos.yaml URL for a src like
// "github.com/owner/repo" (a leading scheme and a trailing .git are tolerated).
func manifestURL(base, src string) (string, error) {
	s := strings.TrimSpace(src)
	for _, p := range []string{"https://", "http://", "github.com/"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("src %q must look like github.com/owner/repo", src)
	}
	return fmt.Sprintf("%s/%s/%s/HEAD/mitos.yaml", base, parts[0], parts[1]), nil
}

// Fetch retrieves and parses the repo's mitos.yaml.
func (g *GitHubFetcher) Fetch(ctx context.Context, src string) (*runmanifest.Manifest, error) {
	u, err := manifestURL(g.base(), src)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c := g.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("no mitos.yaml at %s; add one to the repo root to make it runnable", u)
	default:
		return nil, fmt.Errorf("fetch %s: unexpected status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", u, err)
	}
	return runmanifest.Parse(body)
}
