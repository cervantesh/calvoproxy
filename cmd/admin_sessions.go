package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	adminSessionCookie = "calvoproxy_admin"
	adminSessionTTL    = 8 * time.Hour
	adminLoginWindow   = time.Minute
	adminLoginBurst    = 5
)

type adminSession struct {
	csrf      string
	expiresAt time.Time
}

type adminLoginAttempts struct {
	windowStart time.Time
	count       int
}

type adminSessions struct {
	mu       sync.Mutex
	sessions map[[sha256.Size]byte]adminSession
	attempts map[string]adminLoginAttempts
	now      func() time.Time
	random   io.Reader
}

func newAdminSessions() *adminSessions {
	return &adminSessions{
		sessions: make(map[[sha256.Size]byte]adminSession),
		attempts: make(map[string]adminLoginAttempts),
		now:      time.Now,
		random:   rand.Reader,
	}
}

func adminToken() string { return strings.TrimSpace(os.Getenv("PROXY_ADMIN_TOKEN")) }

func secureEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func (s *adminSessions) issue() (token, csrf string, expires time.Time, err error) {
	raw := make([]byte, 32)
	csrfRaw := make([]byte, 32)
	if _, err = io.ReadFull(s.random, raw); err != nil {
		return "", "", time.Time{}, err
	}
	if _, err = io.ReadFull(s.random, csrfRaw); err != nil {
		return "", "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	csrf = base64.RawURLEncoding.EncodeToString(csrfRaw)
	expires = s.now().Add(adminSessionTTL)
	hash := sha256.Sum256([]byte(token))

	s.mu.Lock()
	s.cleanupLocked()
	s.sessions[hash] = adminSession{csrf: csrf, expiresAt: expires}
	s.mu.Unlock()
	return token, csrf, expires, nil
}

func (s *adminSessions) get(token string) (adminSession, bool) {
	if token == "" {
		return adminSession{}, false
	}
	hash := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	session, ok := s.sessions[hash]
	return session, ok
}

func (s *adminSessions) revoke(token string) {
	hash := sha256.Sum256([]byte(token))
	s.mu.Lock()
	delete(s.sessions, hash)
	s.mu.Unlock()
}

func (s *adminSessions) cleanupLocked() {
	now := s.now()
	for hash, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, hash)
		}
	}
	if len(s.sessions) > 2048 {
		for hash := range s.sessions {
			delete(s.sessions, hash)
			if len(s.sessions) <= 1024 {
				break
			}
		}
	}
}

func (s *adminSessions) allowLogin(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry := s.attempts[ip]
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= adminLoginWindow {
		entry = adminLoginAttempts{windowStart: now}
	}
	entry.count++
	s.attempts[ip] = entry
	if len(s.attempts) > 2048 {
		for key, candidate := range s.attempts {
			if now.Sub(candidate.windowStart) >= adminLoginWindow {
				delete(s.attempts, key)
			}
		}
	}
	return entry.count <= adminLoginBurst
}

func adminClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func adminTransportAllowed(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(os.Getenv("PROXY_ADMIN_ALLOW_INSECURE_REMOTE")), "true") {
		return true
	}
	ip := net.ParseIP(adminClientIP(r))
	return ip != nil && ip.IsLoopback()
}

func adminExpectedOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func adminOriginValid(r *http.Request) bool {
	origin := strings.TrimSuffix(strings.TrimSpace(r.Header.Get("Origin")), "/")
	return origin != "" && origin == strings.TrimSuffix(adminExpectedOrigin(r), "/")
}

func setAdminSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeAdminJSON(w, status, map[string]string{"error": message})
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		writeAdminError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeAdminError(w, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	var extra any
	err := decoder.Decode(&extra)
	if !errors.Is(err, io.EOF) {
		writeAdminError(w, http.StatusBadRequest, "JSON body must contain one value")
		return false
	}
	return true
}

func rejectAdminBody(w http.ResponseWriter, r *http.Request, operation string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1)
	buffer := make([]byte, 1)
	count, err := r.Body.Read(buffer)
	if count != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		writeAdminError(w, http.StatusBadRequest, operation+" does not accept a request body")
		return false
	}
	return true
}

func adminBearerAuthorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	want := adminToken()
	return want != "" && secureEqual(strings.TrimPrefix(header, "Bearer "), want)
}

func (s *adminSessions) authenticate(r *http.Request, mutation bool) (adminSession, bool) {
	if adminBearerAuthorized(r) {
		return adminSession{}, true
	}
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return adminSession{}, false
	}
	session, ok := s.get(cookie.Value)
	if !ok {
		return adminSession{}, false
	}
	if mutation && (!adminOriginValid(r) || !secureEqual(r.Header.Get("X-CSRF-Token"), session.csrf)) {
		return adminSession{}, false
	}
	return session, true
}

func (s *adminSessions) sessionHandler(w http.ResponseWriter, r *http.Request) {
	if adminToken() == "" {
		writeAdminError(w, http.StatusServiceUnavailable, "Admin UI requires PROXY_ADMIN_TOKEN")
		return
	}
	switch r.Method {
	case http.MethodPost:
		if !adminOriginValid(r) {
			writeAdminError(w, http.StatusForbidden, "Invalid request origin")
			return
		}
		if !s.allowLogin(adminClientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeAdminError(w, http.StatusTooManyRequests, "Too many login attempts; try again shortly")
			return
		}
		var input struct {
			Token string `json:"token"`
		}
		if !decodeAdminJSON(w, r, 4096, &input) {
			return
		}
		if !secureEqual(input.Token, adminToken()) {
			writeAdminError(w, http.StatusUnauthorized, "Invalid admin token")
			return
		}
		token, csrf, expires, err := s.issue()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "Could not create admin session")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Value: token, Path: "/admin", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(adminSessionTTL.Seconds())})
		writeAdminJSON(w, http.StatusCreated, map[string]string{"csrf_token": csrf})
	case http.MethodDelete:
		if _, ok := s.authenticate(r, true); !ok {
			writeAdminError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if cookie, err := r.Cookie(adminSessionCookie); err == nil {
			s.revoke(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: adminSessionCookie, Path: "/admin", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeAdminError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}
