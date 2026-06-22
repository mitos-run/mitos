// Package spa embeds the built console SPA (web/app/dist) into the console
// binary so a SINGLE image serves both the BFF and the UI — no Node at runtime,
// one Helm Deployment. The Dockerfile builds the SPA and copies its dist over
// the committed placeholder before `go build`, so the embed always has content.
package spa

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler serves the embedded SPA: static assets by path, with a fallback to
// index.html for any non-asset path so client-side routing works on deep links.
// A request that looks like an asset (has a file extension) but is missing
// returns 404 rather than masking it with index.html.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			if strings.Contains(pathBase(p), ".") {
				http.NotFound(w, r) // a missing asset is a 404, not the app shell
				return
			}
			// Unknown route → serve the app shell for client-side routing.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			files.ServeHTTP(w, r2)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
