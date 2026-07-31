package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
