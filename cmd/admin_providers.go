package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cervantesh/calvoproxy/internal/secretstore"
)

//go:embed adminui/index.html adminui/app.js adminui/styles.css
var adminUIFiles embed.FS

type adminProviderServer struct {
	store    secretstore.Store
	sessions *adminSessions
	client   *http.Client
}

// MountAdminProviders installs the self-contained provider-key console and API.
// The caller retains ownership of store; this function deliberately does not
// modify proxy routing or process configuration.
func MountAdminProviders(mux *http.ServeMux, store secretstore.Store) {
	server := &adminProviderServer{
		store:    store,
		sessions: newAdminSessions(),
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	mux.HandleFunc("/admin/providers", server.protected(server.servePage))
	mux.HandleFunc("/admin/assets/", server.protected(server.serveAsset))
	mux.HandleFunc("/admin/session", server.protected(server.sessions.sessionHandler))
	mux.HandleFunc("/admin/api/providers", server.protected(server.providersHandler))
	mux.HandleFunc("/admin/api/providers/", server.protected(server.providerActionHandler))
}

func (s *adminProviderServer) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setAdminSecurityHeaders(w)
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !adminTransportAllowed(r) {
			writeAdminError(w, http.StatusForbidden, "Admin UI requires HTTPS for remote access")
			return
		}
		if adminToken() == "" {
			writeAdminError(w, http.StatusServiceUnavailable, "Admin UI requires PROXY_ADMIN_TOKEN")
			return
		}
		next(w, r)
	}
}

func (s *adminProviderServer) servePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	data, err := adminUIFiles.ReadFile("adminui/index.html")
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "Admin UI is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (s *adminProviderServer) serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/assets/")
	contentType := ""
	switch name {
	case "app.js":
		contentType = "text/javascript; charset=utf-8"
	case "styles.css":
		contentType = "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := adminUIFiles.ReadFile("adminui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

type adminProviderView struct {
	Provider   secretstore.Provider `json:"provider"`
	Configured bool                 `json:"configured"`
	Source     string               `json:"source"`
	Active     bool                 `json:"managed_active"`
	Status     string               `json:"status"`
}

func (s *adminProviderServer) providersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	session, ok := s.sessions.authenticate(r, false)
	if !ok {
		writeAdminError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	snapshot := s.store.Status(r.Context())
	views := make([]adminProviderView, 0, len(snapshot.Providers))
	for _, status := range snapshot.Providers {
		view := adminProviderView{Provider: status.Provider, Configured: status.Configured, Source: "none", Status: "missing"}
		if adminProviderEnvironmentKey(status.Provider) != "" {
			view.Source = "environment"
			view.Status = "configured"
			views = append(views, view)
			continue
		}
		if status.Configured {
			view.Source = "vault"
			view.Active = true
			view.Status = "configured"
		}
		views = append(views, view)
	}
	response := map[string]any{
		"backend":    snapshot.Backend,
		"available":  snapshot.Available,
		"locked":     snapshot.Locked,
		"providers":  views,
		"csrf_token": session.csrf,
	}
	writeAdminJSON(w, http.StatusOK, response)
}

func zeroAdminSecret(secret []byte) {
	for index := range secret {
		secret[index] = 0
	}
}

func parseAdminProviderAction(path string) (secretstore.Provider, string, bool) {
	remainder := strings.TrimPrefix(path, "/admin/api/providers/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	provider := secretstore.Provider(parts[0])
	switch provider {
	case secretstore.ProviderOpenRouter, secretstore.ProviderCerebras, secretstore.ProviderGroq:
	default:
		return "", "", false
	}
	if parts[1] != "key" && parts[1] != "test" {
		return "", "", false
	}
	return provider, parts[1], true
}

func (s *adminProviderServer) providerActionHandler(w http.ResponseWriter, r *http.Request) {
	provider, action, ok := parseAdminProviderAction(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.sessions.authenticate(r, r.Method != http.MethodGet); !ok {
		writeAdminError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	switch action {
	case "key":
		s.keyHandler(w, r, provider)
	case "test":
		s.testHandler(w, r, provider)
	}
}

func (s *adminProviderServer) keyHandler(w http.ResponseWriter, r *http.Request, provider secretstore.Provider) {
	switch r.Method {
	case http.MethodPut:
		var input struct {
			Key string `json:"key"`
		}
		if !decodeAdminJSON(w, r, 8192, &input) {
			return
		}
		secret := []byte(input.Key)
		input.Key = ""
		defer zeroAdminSecret(secret)
		if err := s.store.Set(r.Context(), provider, secret); err != nil {
			writeAdminError(w, http.StatusBadRequest, "The provider key could not be stored")
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"provider": provider, "configured": true})
	case http.MethodDelete:
		if !rejectAdminBody(w, r, "DELETE") {
			return
		}
		if err := s.store.Delete(r.Context(), provider); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "The provider key could not be removed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (s *adminProviderServer) testHandler(w http.ResponseWriter, r *http.Request, provider secretstore.Provider) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if !rejectAdminBody(w, r, "Test") {
		return
	}
	secret := []byte(adminProviderEnvironmentKey(provider))
	if len(secret) == 0 {
		var found bool
		var err error
		secret, found, err = s.store.Get(r.Context(), provider)
		if err != nil || !found {
			zeroAdminSecret(secret)
			writeAdminError(w, http.StatusConflict, "No usable key is configured for this provider")
			return
		}
	}
	defer zeroAdminSecret(secret)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, adminProviderTestURL(provider), nil)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "Provider test could not be started")
		return
	}
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("Accept", "application/json")
	start := time.Now()
	response, err := s.client.Do(request)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "Provider is unreachable or timed out")
		return
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status, message := sanitizeProviderTestFailure(response.StatusCode)
		writeAdminError(w, status, message)
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"provider": provider, "ok": true, "latency_ms": latency})
}

func adminProviderEnvironmentKey(provider secretstore.Provider) string {
	var name string
	switch provider {
	case secretstore.ProviderOpenRouter:
		name = "OPENROUTER_API_KEY"
	case secretstore.ProviderCerebras:
		name = "CEREBRAS_API_KEY"
	case secretstore.ProviderGroq:
		name = "GROQ_API_KEY"
	}
	return strings.TrimSpace(os.Getenv(name))
}

func adminProviderTestURL(provider secretstore.Provider) string {
	envName := "PROXY_ADMIN_" + strings.ToUpper(string(provider)) + "_TEST_URL"
	if override := strings.TrimSpace(os.Getenv(envName)); override != "" {
		return override
	}
	switch provider {
	case secretstore.ProviderCerebras:
		return "https://api.cerebras.ai/v1/models"
	case secretstore.ProviderGroq:
		return "https://api.groq.com/openai/v1/models"
	default:
		return "https://openrouter.ai/api/v1/models"
	}
}

func sanitizeProviderTestFailure(status int) (int, string) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway, "Provider rejected the configured key"
	case http.StatusTooManyRequests:
		return http.StatusBadGateway, "Provider accepted the request but is currently rate-limited"
	default:
		return http.StatusBadGateway, fmt.Sprintf("Provider returned an unexpected status (%d)", status)
	}
}
