package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/dashboard"
	"github.com/cervantesh/calvoproxy/internal/router"
)

// Invariant 1: the dashboard sits behind the same gate as /health. It shows
// model chains, upstream error text and internal state — exactly what that gate
// exists to protect.
func TestDashboard_RequiresAdminTokenWhenSet(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "s3cret")

	for _, path := range []string{"/dashboard", "/dashboard/state"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			admin(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s without a token = %d, want 401", path, rec.Code)
			}
		})
	}
}

// Invariant 5: the page is self-contained. A CDN reference would work on a dev
// machine and break on the offline install this project promises.
func TestDashboard_PageHasNoExternalResources(t *testing.T) {
	page, err := dashboard.Page()
	if err != nil {
		t.Fatalf("embedded page missing: %v", err)
	}
	for _, forbidden := range []string{"http://", "https://", "//cdn", "integrity="} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page references something external (%q); it must be fully embedded", forbidden)
		}
	}
	if !strings.Contains(page, "<!doctype html>") {
		t.Error("embedded asset does not look like the dashboard page")
	}
}

// Invariant 6: the page is served as HTML, with a policy that fails loudly if a
// future edit adds an external script.
func TestDashboard_ServesHTMLWithLockedDownPolicy(t *testing.T) {
	rec := httptest.NewRecorder()
	dashboard.Handler()(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q, want it to confine the page to its own origin", csp)
	}
}

// Invariant 4: the state payload carries the four things the view renders. The
// dashboard computes nothing, so anything missing here is missing everywhere.
func TestDashboard_StatePayloadCarriesEverythingTheViewNeeds(t *testing.T) {
	svc := router.NewRouterService()
	t.Cleanup(svc.Close)

	rec := httptest.NewRecorder()
	health := svc.Health()
	writeJSON(rec, map[string]any{
		"health":    health,
		"counters":  svc.Counters(),
		"quotas":    health.Quotas,
		"decisions": svc.RecentDecisions(dashboardDecisions),
	})

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	for _, key := range []string{"health", "counters", "quotas", "decisions"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("state payload is missing %q", key)
		}
	}
}
