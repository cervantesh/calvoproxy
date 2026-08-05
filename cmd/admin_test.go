package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router"
)

func TestAdminGate(t *testing.T) {
	call := func(hdr string) (int, bool) {
		got := false
		h := admin(func(w http.ResponseWriter, r *http.Request) { got = true; w.WriteHeader(200) })
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		if hdr != "" {
			req.Header.Set("Authorization", "Bearer "+hdr)
		}
		h(rec, req)
		return rec.Code, got
	}
	// No token configured -> open.
	if code, called := call(""); code != 200 || !called {
		t.Fatalf("no-token should be open: code=%d called=%v", code, called)
	}
	// Token configured -> gated.
	t.Setenv("PROXY_ADMIN_TOKEN", "sec")
	if code, called := call(""); code != http.StatusUnauthorized || called {
		t.Fatalf("expected 401 without token: code=%d called=%v", code, called)
	}
	if code, called := call("wrong"); code != http.StatusUnauthorized || called {
		t.Fatalf("expected 401 with wrong token: code=%d", code)
	}
	if code, called := call("sec"); code != 200 || !called {
		t.Fatalf("expected pass with correct token: code=%d", code)
	}
}

// /decisions/{id} is the admin-gated channel for a routing decision's detail
// (spec §5). An id the ring no longer holds is a 404, not an error worth
// distinguishing: the buffer is bounded on purpose.
func TestDecisionsEndpointIsAdminGatedAnd404sUnknownIds(t *testing.T) {
	svc := router.NewRouterService()
	t.Cleanup(svc.Close)
	mux := newMux(svc, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decisions/ffffffffffffffff", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown decision id should be 404, got %d: %s", rec.Code, rec.Body.String())
	}

	t.Setenv("PROXY_ADMIN_TOKEN", "secret")
	gated := httptest.NewRecorder()
	mux.ServeHTTP(gated, httptest.NewRequest(http.MethodGet, "/decisions/ffffffffffffffff", nil))
	if gated.Code != http.StatusUnauthorized {
		t.Errorf("decision detail carries upstream error text and must be gated, got %d", gated.Code)
	}
}
