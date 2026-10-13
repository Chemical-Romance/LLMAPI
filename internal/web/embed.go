package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:index.html
//go:embed all:css
//go:embed all:js
var assets embed.FS

// Handler serves the embedded admin console. The caller mounts it under
// /console. Client-side SPA routes (paths without a dot) fall back to
// index.html so the UI can deep-link.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, ".")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/console")
		if p == "" {
			p = "/"
		}
		// client-side route -> serve the directory index (index.html).
		// Rewriting to "/index.html" would make FileServer 301 to "./",
		// causing an infinite redirect loop under /console/.
		if !strings.Contains(path.Base(p), ".") {
			p = "/"
		}
		r.URL.Path = p
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
