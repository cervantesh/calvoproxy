package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/secretstore"
)

type adminTestStore struct {
	mu      sync.Mutex
	values  map[secretstore.Provider][]byte
	backend string
	err     error
	gets    int
}

func newAdminTestStore() *adminTestStore {
	return &adminTestStore{values: make(map[secretstore.Provider][]byte), backend: "test-vault"}
}

func (s *adminTestStore) Get(_ context.Context, provider secretstore.Provider) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.err != nil {
		return nil, false, s.err
	}
	value, ok := s.values[provider]
	return append([]byte(nil), value...), ok, nil
}

func (s *adminTestStore) Set(_ context.Context, provider secretstore.Provider, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.values[provider] = append([]byte(nil), value...)
	return nil
}

func (s *adminTestStore) Delete(_ context.Context, provider secretstore.Provider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	delete(s.values, provider)
	return nil
}

func (s *adminTestStore) Status(context.Context) secretstore.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make([]secretstore.ProviderStatus, 0, 3)
	for _, provider := range []secretstore.Provider{secretstore.ProviderOpenRouter, secretstore.ProviderCerebras, secretstore.ProviderGroq} {
		_, ok := s.values[provider]
		statuses = append(statuses, secretstore.ProviderStatus{Provider: provider, Configured: ok})
	}
	return secretstore.Snapshot{Backend: s.backend, Available: s.err == nil, Locked: s.err != nil, Providers: statuses}
}

func adminTestMux(store secretstore.Store) *http.ServeMux {
	mux := http.NewServeMux()
	MountAdminProviders(mux, store)
	return mux
}

func adminRequest(method, target, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:23001"
	return request
}

func loginAdmin(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	request := adminRequest(http.MethodPost, "http://example.test/admin/session", `{"token":"correct horse"}`)
	request.Header.Set("Origin", "http://example.test")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("login status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	result := recorder.Result()
	cookies := result.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	return cookies[0], response.CSRF
}

func TestAdminUIRequiresConfiguredTokenAndSecureRemoteTransport(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "")
	handler := adminTestMux(newAdminTestStore())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, adminRequest(http.MethodGet, "http://example.test/admin/providers", ""))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing token status = %d", recorder.Code)
	}

	t.Setenv("PROXY_ADMIN_TOKEN", "secret")
	request := adminRequest(http.MethodGet, "http://example.test/admin/providers", "")
	request.RemoteAddr = "100.64.1.2:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote HTTP status = %d", recorder.Code)
	}
}

func TestAdminUIIsEmbeddedAndHardened(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "secret")
	recorder := httptest.NewRecorder()
	adminTestMux(newAdminTestStore()).ServeHTTP(recorder, adminRequest(http.MethodGet, "http://example.test/admin/providers", ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Provider API keys") {
		t.Fatalf("page status/body = %d %q", recorder.Code, recorder.Body.String())
	}
	for _, header := range []string{"Content-Security-Policy", "Cache-Control", "X-Content-Type-Options", "X-Frame-Options"} {
		if recorder.Header().Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
	if strings.Contains(recorder.Body.String(), "<script>") || strings.Contains(recorder.Body.String(), "style=") {
		t.Error("UI must not contain inline script or style")
	}
}

func TestAdminLoginSessionCookieAndProviderLifecycle(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	t.Setenv("OPENROUTER_API_KEY", "")
	store := newAdminTestStore()
	handler := adminTestMux(store)
	cookie, csrf := loginAdmin(t, handler)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" || len(cookie.Value) < 40 {
		t.Fatalf("weak session cookie: %#v", cookie)
	}

	put := adminRequest(http.MethodPut, "http://example.test/admin/api/providers/openrouter/key", `{"key":"sk-secret-1234"}`)
	put.Header.Set("Content-Type", "application/json")
	put.Header.Set("Origin", "http://example.test")
	put.Header.Set("X-CSRF-Token", csrf)
	put.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, put)
	if recorder.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-secret") {
		t.Fatal("PUT response leaked full secret")
	}

	get := adminRequest(http.MethodGet, "http://example.test/admin/api/providers", "")
	get.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, get)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"source":"vault"`) {
		t.Fatalf("GET status/body = %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sk-secret") || strings.Contains(recorder.Body.String(), "1234") || strings.Contains(recorder.Body.String(), "last4") {
		t.Fatal("GET response leaked secret-derived metadata")
	}

	deleteRequest := adminRequest(http.MethodDelete, "http://example.test/admin/api/providers/openrouter/key", "")
	deleteRequest.Header.Set("Origin", "http://example.test")
	deleteRequest.Header.Set("X-CSRF-Token", csrf)
	deleteRequest.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, deleteRequest)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, found, _ := store.Get(context.Background(), secretstore.ProviderOpenRouter); found {
		t.Fatal("key remained after DELETE")
	}
}

func TestAdminProviderListDoesNotReadSecretsAndShowsEnvironmentOverride(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	t.Setenv("OPENROUTER_API_KEY", "environment-secret")
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	store := newAdminTestStore()
	store.values[secretstore.ProviderOpenRouter] = []byte("vault-secret")
	handler := adminTestMux(store)
	request := adminRequest(http.MethodGet, "http://example.test/admin/api/providers", "")
	request.Header.Set("Authorization", "Bearer correct horse")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if store.gets != 0 {
		t.Fatalf("provider listing read %d secrets", store.gets)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"source":"environment"`) || !strings.Contains(body, `"managed_active":false`) {
		t.Fatalf("environment override not represented: %s", body)
	}
	if strings.Contains(body, "environment-secret") || strings.Contains(body, "vault-secret") {
		t.Fatal("provider listing leaked a secret")
	}
}

func TestAdminMutationsRequireCSRFForCookieButNotBearer(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	store := newAdminTestStore()
	handler := adminTestMux(store)
	cookie, _ := loginAdmin(t, handler)

	request := adminRequest(http.MethodPut, "http://example.test/admin/api/providers/groq/key", `{"key":"gsk_test_1234"}`)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.test")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing CSRF status = %d", recorder.Code)
	}

	request = adminRequest(http.MethodPut, "http://example.test/admin/api/providers/groq/key", `{"key":"gsk_test_1234"}`)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer correct horse")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bearer mutation status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminRejectsBadJSONUnknownProviderAndTrailingData(t *testing.T) {
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	handler := adminTestMux(newAdminTestStore())
	for _, test := range []struct {
		path string
		body string
		want int
	}{
		{"/admin/api/providers/other/key", `{"key":"secret"}`, http.StatusNotFound},
		{"/admin/api/providers/groq/key", `{"key":"secret"} {}`, http.StatusBadRequest},
		{"/admin/api/providers/groq/key", `{"key":"secret","extra":true}`, http.StatusBadRequest},
	} {
		request := adminRequest(http.MethodPut, "http://example.test"+test.path, test.body)
		request.Header.Set("Authorization", "Bearer correct horse")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.want {
			t.Errorf("%s status = %d, want %d: %s", test.path, recorder.Code, test.want, recorder.Body.String())
		}
	}
}

func TestAdminProviderTestIsCappedAndSanitized(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer environment-key" {
			t.Errorf("provider request did not use environment override: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, strings.Repeat("sensitive provider detail ", 5000))
	}))
	defer provider.Close()
	t.Setenv("PROXY_ADMIN_TOKEN", "correct horse")
	t.Setenv("PROXY_ADMIN_GROQ_TEST_URL", provider.URL)
	t.Setenv("GROQ_API_KEY", "environment-key")
	store := newAdminTestStore()
	store.values[secretstore.ProviderGroq] = []byte("test-key")
	handler := adminTestMux(store)
	request := adminRequest(http.MethodPost, "http://example.test/admin/api/providers/groq/test", "")
	request.Header.Set("Authorization", "Bearer correct horse")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "rejected") {
		t.Fatalf("test status/body = %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sensitive provider detail") {
		t.Fatal("raw provider body leaked")
	}
}
