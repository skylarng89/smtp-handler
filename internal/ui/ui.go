// Package ui serves the embedded admin web UI (a Vue single-page app built
// into dist/ by `make ui`). If the UI was not built into this binary it
// serves an explanatory page instead, so Go-only builds still work.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Prefix is where the UI is mounted.
const Prefix = "/ui/"

// csp keeps the admin app self-contained. Email previews render inside a
// sandboxed iframe that inherits this policy, so remote images (tracking
// pixels) and scripts in stored emails are blocked.
const csp = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Built reports whether a compiled UI is embedded.
func Built() bool {
	_, err := fs.Stat(distFS, "dist/index.html")
	return err == nil
}

// Handler serves the UI under Prefix with single-page-app fallback.
func Handler() http.Handler {
	root, _ := fs.Sub(distFS, "dist")
	files := http.FileServerFS(root)

	return http.StripPrefix(strings.TrimSuffix(Prefix, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")

		if !Built() {
			h.Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(notBuiltPage))
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if info, err := fs.Stat(root, name); err == nil && !info.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r) // a missing asset must not fall back to HTML
				return
			}
		}
		// Client-side route: serve the app shell.
		h.Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	}))
}

const notBuiltPage = `<!doctype html><meta charset="utf-8"><title>SMTP Handler</title>
<body style="font:16px system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>Admin UI not built</h1>
<p>This binary was compiled without the web UI. Run <code>make ui build</code> (needs Node.js),
or use the official container image, which includes it. The admin API under
<code>/admin/api/v1</code> works without the UI.</p></body>`
