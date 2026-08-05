// Package dashboard serves a read-only view of what the proxy is doing right
// now: circuits, scores, quota budgets and the last N routing decisions.
//
// It is a VIEW and nothing more. It computes no aggregate of its own — anything
// it shows must already exist as a snapshot on the router, the same discipline
// /metrics follows. When something is missing, it gets added to the router and
// tested there, never derived here where no test would see it.
//
// The page is embedded and self-contained: no CDN, no build step, no framework.
// The binary has to keep compiling offline with -mod=vendor, and a dashboard
// that fetches a script from the internet would break the one property this
// project is strictest about.
package dashboard

import (
	"embed"
	"net/http"
)

//go:embed assets/index.html
var assets embed.FS

// Handler serves the embedded page.
func Handler() http.HandlerFunc {
	page, err := assets.ReadFile("assets/index.html")
	return func(w http.ResponseWriter, r *http.Request) {
		if err != nil {
			http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page only ever talks to its own origin, and saying so costs one
		// header: if a future edit accidentally adds a CDN script, it fails
		// loudly in the browser instead of quietly working on the dev's machine
		// and breaking on an offline install.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		_, _ = w.Write(page)
	}
}

// Page returns the embedded HTML, for tests that assert on its content.
func Page() (string, error) {
	page, err := assets.ReadFile("assets/index.html")
	return string(page), err
}
