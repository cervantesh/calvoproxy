package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminSessionStoresOnlyHashAndExpires(t *testing.T) {
	sessions := newAdminSessions()
	now := time.Date(2026, 8, 9, 1, 0, 0, 0, time.UTC)
	sessions.now = func() time.Time { return now }
	token, csrf, _, err := sessions.issue()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || csrf == "" || token == csrf {
		t.Fatal("session and CSRF must be independent opaque values")
	}
	for hash := range sessions.sessions {
		if strings.Contains(string(hash[:]), token) {
			t.Fatal("raw token stored in session map")
		}
	}
	if _, ok := sessions.get(token); !ok {
		t.Fatal("fresh session missing")
	}
	now = now.Add(adminSessionTTL + time.Second)
	if _, ok := sessions.get(token); ok {
		t.Fatal("expired session accepted")
	}
}

func TestAdminLoginRateLimitAndStrictOrigin(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	handler := adminTestMux(newAdminTestStore())

	badOrigin := adminRequest(http.MethodPost, "http://example.test/admin/session", `{"token":"correct horse"}`)
	badOrigin.Header.Set("Content-Type", "application/json")
	badOrigin.Header.Set("Origin", "http://evil.test")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, badOrigin)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("bad origin status = %d", recorder.Code)
	}

	for attempt := 1; attempt <= adminLoginBurst+1; attempt++ {
		request := adminRequest(http.MethodPost, "http://example.test/admin/session", `{"token":"wrong"}`)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://example.test")
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if attempt <= adminLoginBurst && recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", attempt, recorder.Code)
		}
		if attempt > adminLoginBurst && recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("rate limit status = %d", recorder.Code)
		}
	}
}

func TestAdminSessionCookieUsesSecureOnTLS(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	request := httptest.NewRequest(http.MethodPost, "https://example.test/admin/session", strings.NewReader(`{"token":"correct horse"}`))
	request.RemoteAddr = "203.0.113.2:4567"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://example.test")
	recorder := httptest.NewRecorder()
	adminTestMux(newAdminTestStore()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatalf("TLS session cookie not Secure: %#v", cookies)
	}
}
