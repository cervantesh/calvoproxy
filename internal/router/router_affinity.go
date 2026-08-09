package router

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	headerCalvoProxySession = "X-Calvoproxy-Session-Id"
	headerClaudeCodeSession = "X-Claude-Code-Session-Id"
	headerGenericSession    = "X-Session-Id"
	headerOpenCodeSession   = "X-Opencode-Session"
)

type affinityContextKey struct{}

type affinityRoute struct {
	Provider providerID
	Model    string
	usedAt   time.Time
}

type affinityCacheEntry struct {
	key   string
	route affinityRoute
}

type affinityStore struct {
	mu         sync.Mutex
	secret     []byte
	ttl        time.Duration
	maxEntries int
	entries    map[string]*list.Element
	recency    *list.List
	now        func() time.Time
}

func randomAffinitySecret() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// A process-local random UUID-quality salt is preferred, but hashing a
		// high-resolution timestamp still keeps raw session IDs out of memory and
		// logs if the operating system RNG is temporarily unavailable.
		sum := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
		return sum[:]
	}
	return secret
}

func newAffinityStore(secret []byte, ttl time.Duration, maxEntries int) *affinityStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries < 1 {
		maxEntries = 8192
	}
	return &affinityStore{
		secret: append([]byte(nil), secret...), ttl: ttl, maxEntries: maxEntries,
		entries: make(map[string]*list.Element), recency: list.New(), now: time.Now,
	}
}

func (s *affinityStore) key(sessionID, credential string) string {
	if s == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(strings.TrimSpace(credential)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.TrimSpace(sessionID)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *affinityStore) keyForRequest(r *http.Request, credential string) string {
	if s == nil || r == nil {
		return ""
	}
	for _, header := range []string{headerCalvoProxySession, headerClaudeCodeSession, headerGenericSession, headerOpenCodeSession} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return s.key(value, credential)
		}
	}
	return ""
}

func (s *affinityStore) preferred(key string) (affinityRoute, bool) {
	if s == nil || key == "" {
		return affinityRoute{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.entries[key]
	if !ok {
		return affinityRoute{}, false
	}
	entry := element.Value.(affinityCacheEntry)
	now := s.now()
	if now.Sub(entry.route.usedAt) > s.ttl {
		delete(s.entries, key)
		s.recency.Remove(element)
		return affinityRoute{}, false
	}
	entry.route.usedAt = now
	element.Value = entry
	s.recency.MoveToFront(element)
	return entry.route, true
}

func (s *affinityStore) pin(key string, attempt modelAttempt) {
	if s == nil || key == "" || attempt.Provider == "" || strings.TrimSpace(attempt.Model) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if element, ok := s.entries[key]; ok {
		element.Value = affinityCacheEntry{key: key, route: affinityRoute{Provider: attempt.Provider, Model: attempt.Model, usedAt: now}}
		s.recency.MoveToFront(element)
		return
	}
	element := s.recency.PushFront(affinityCacheEntry{key: key, route: affinityRoute{Provider: attempt.Provider, Model: attempt.Model, usedAt: now}})
	s.entries[key] = element
	if len(s.entries) <= s.maxEntries {
		return
	}
	oldest := s.recency.Back()
	if oldest != nil {
		delete(s.entries, oldest.Value.(affinityCacheEntry).key)
		s.recency.Remove(oldest)
	}
}

func (s *affinityStore) forget(key string, attempt modelAttempt) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.entries[key]
	if !ok {
		return
	}
	entry := element.Value.(affinityCacheEntry)
	if entry.route.Provider == attempt.Provider && entry.route.Model == attempt.Model {
		delete(s.entries, key)
		s.recency.Remove(element)
	}
}

func withAffinityKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, affinityContextKey{}, key)
}

func affinityKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(affinityContextKey{}).(string)
	return key
}

func (s *RouterService) applySessionAffinity(ctx context.Context, attempts []modelAttempt) []modelAttempt {
	key := affinityKeyFrom(ctx)
	preferred, ok := s.affinity.preferred(key)
	if !ok {
		return attempts
	}
	for i, attempt := range attempts {
		if attempt.Provider == preferred.Provider && attempt.Model == preferred.Model {
			if i == 0 {
				return attempts
			}
			out := append([]modelAttempt(nil), attempts...)
			copy(out[1:i+1], out[0:i])
			out[0] = attempt
			return out
		}
	}
	return attempts
}

func (s *RouterService) recordAffinitySuccess(ctx context.Context, attempt modelAttempt) {
	s.affinity.pin(affinityKeyFrom(ctx), attempt)
}

func (s *RouterService) recordAffinityFailure(ctx context.Context, attempt modelAttempt) {
	s.affinity.forget(affinityKeyFrom(ctx), attempt)
}
