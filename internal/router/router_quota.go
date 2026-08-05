package router

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The quota ledger is the predictive half of resilience; the breaker is the
// reactive half.
//
// Free-tier limits used to be discovered by hitting them: a 429 arrived, the
// circuit opened, and the request was already spent. The breaker is reactive by
// design and should stay that way. What was missing is knowing how much of the
// window is left and degrading BEFORE it runs out.
//
// Two things about this file are load-bearing and easy to get wrong:
//
// Scope keys are NOT breakerKey. breakerKey is profile+":"+model, which is right
// for reliability — the same model under `coding` and `bulk` sees different load
// and deserves its own circuit and score. It is fatal for quota: OpenRouter
// charges per account and per model, so two partial counters of the same pocket
// would each see a fraction of the traffic and never detect exhaustion.
//
// The ledger has its own mutex and never calls back into the breaker. Lock order
// is breakerMu → quotaMu, because isModelAvailableLocked is reached from Health()
// with breakerMu already held.

const (
	quotaScopeAccount = "account"
	// minuteSuffix marks a scope's per-minute window, kept beside its daily one.
	// The daily budget is what runs out; the per-minute one is what trips first
	// during a burst, and they expire on completely different clocks.
	minuteSuffix = "#m"
)

// quotaLimit is what an operator (or an upstream header) says fits in a window.
type quotaLimit struct {
	RPD int64 `json:"rpd,omitempty"`
	RPM int64 `json:"rpm,omitempty"`
}

type quotaWindow struct {
	Limit   int64
	Used    int64
	ResetAt time.Time
	// Source records where the limit came from: config, header or learned.
	// Nothing infers a ceiling — see learnFrom429.
	Source string
	daily  bool
}

type quotaLedger struct {
	mu     sync.RWMutex
	limits map[string]quotaLimit
	scopes map[string]*quotaWindow
	dirty  bool
}

func newQuotaLedger(limits map[string]quotaLimit) *quotaLedger {
	if limits == nil {
		limits = map[string]quotaLimit{}
	}
	return &quotaLedger{limits: limits, scopes: map[string]*quotaWindow{}}
}

// quotaLimitsFromEnv reads PROXY_QUOTA_LIMITS_JSON. Configuration lives in the
// environment rather than model-policy.json because that file's shape is owned
// by the vendored policy package. Malformed input is ignored with a warning:
// quota is an optimisation, and a typo in an env var is not a reason to refuse
// traffic.
func quotaLimitsFromEnv() map[string]quotaLimit {
	raw := strings.TrimSpace(envValue("PROXY_QUOTA_LIMITS_JSON"))
	if raw == "" {
		return map[string]quotaLimit{}
	}
	parsed := map[string]quotaLimit{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("[CalvoProxy] PROXY_QUOTA_LIMITS_JSON is not valid JSON; quota limits ignored",
			slog.String("error", err.Error()))
		return map[string]quotaLimit{}
	}
	return parsed
}

func quotaHardSkipEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(envValue("PROXY_QUOTA_HARD_SKIP")), "true")
}

func modelScope(attempt modelAttempt) string {
	return "model:" + strings.TrimSpace(attempt.Model)
}

// nextReset is when a fresh window closes. Daily windows land on the next UTC
// midnight because that is how free tiers roll; minute windows are relative.
func nextReset(daily bool, now time.Time) time.Time {
	if !daily {
		return now.Add(time.Minute)
	}
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}

// windowLocked returns the live window for a scope, rolling it first if its
// reset has passed. Rolling zeroes the count and KEEPS the limit: discarding the
// window would throw away what we know about the ceiling along with the count.
func (l *quotaLedger) windowLocked(scope string, daily bool, now time.Time) *quotaWindow {
	w := l.scopes[scope]
	if w == nil {
		limit := l.configuredLimit(scope, daily)
		w = &quotaWindow{Limit: limit, ResetAt: nextReset(daily, now), daily: daily}
		if limit > 0 {
			w.Source = "config"
		}
		l.scopes[scope] = w
		return w
	}
	if !w.ResetAt.After(now) {
		w.Used = 0
		w.ResetAt = nextReset(w.daily, now)
	}
	return w
}

func (l *quotaLedger) configuredLimit(scope string, daily bool) int64 {
	base := strings.TrimSuffix(scope, minuteSuffix)
	limit, ok := l.limits[base]
	if !ok {
		return 0
	}
	if daily {
		return limit.RPD
	}
	return limit.RPM
}

// record counts one attempt against the model's pocket and the account's.
func (l *quotaLedger) record(attempt modelAttempt) {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, scope := range []string{modelScope(attempt), quotaScopeAccount} {
		l.windowLocked(scope, true, now).Used++
		l.windowLocked(scope+minuteSuffix, false, now).Used++
	}
	l.dirty = true
}

func (l *quotaLedger) used(scope string) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.windowLocked(scope, !strings.HasSuffix(scope, minuteSuffix), time.Now()).Used
}

func (l *quotaLedger) limit(scope string) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.windowLocked(scope, !strings.HasSuffix(scope, minuteSuffix), time.Now()).Limit
}

// headroom is what is left of the tightest window this attempt touches, in
// [0,1]. With no known limit it is 1: nothing pretends to know a ceiling, and
// inventing one would be worse than having none.
func (l *quotaLedger) headroom(attempt modelAttempt) float64 {
	if l == nil {
		return 1
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	lowest := 1.0
	model := modelScope(attempt)
	for _, s := range []struct {
		scope string
		daily bool
	}{
		{model, true}, {model + minuteSuffix, false},
		{quotaScopeAccount, true}, {quotaScopeAccount + minuteSuffix, false},
	} {
		w := l.windowLocked(s.scope, s.daily, now)
		if w.Limit <= 0 {
			continue
		}
		left := float64(w.Limit-w.Used) / float64(w.Limit)
		if left < 0 {
			left = 0
		}
		if left < lowest {
			lowest = left
		}
	}
	return lowest
}

// exhausted reports a hard exclusion, and only when explicitly asked for. The
// default is soft degradation: a hard skip widens the "all models cooling down"
// surface on the strength of limits that may have been learned — and therefore
// may be wrong.
func (l *quotaLedger) exhausted(attempt modelAttempt) bool {
	if l == nil || !quotaHardSkipEnabled() {
		return false
	}
	return l.headroom(attempt) <= 0
}

// retryAfter is how long until this attempt's tightest exhausted window rolls,
// so a quota-driven 503 can tell the client when to come back.
func (l *quotaLedger) retryAfter(attempt modelAttempt) time.Duration {
	if l == nil {
		return 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	var soonest time.Duration
	model := modelScope(attempt)
	for _, s := range []struct {
		scope string
		daily bool
	}{
		{model, true}, {model + minuteSuffix, false},
		{quotaScopeAccount, true}, {quotaScopeAccount + minuteSuffix, false},
	} {
		w := l.windowLocked(s.scope, s.daily, now)
		if w.Limit <= 0 || w.Used < w.Limit {
			continue
		}
		d := w.ResetAt.Sub(now)
		if d > 0 && (soonest == 0 || d < soonest) {
			soonest = d
		}
	}
	return soonest
}

// learnFrom429 records WHEN to come back, never HOW MANY fit. A 429 says "not
// now"; reading a ceiling out of it would be a fabrication, and a fabricated
// ceiling is what turns a helpful gate into an outage.
func (l *quotaLedger) learnFrom429(attempt modelAttempt, retryAfter time.Duration) {
	if l == nil || retryAfter <= 0 {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windowLocked(modelScope(attempt), true, now)
	if reset := now.Add(retryAfter); reset.After(w.ResetAt) || w.Source == "" {
		w.ResetAt = reset
	}
	if w.Source == "" {
		w.Source = "learned"
	}
	l.dirty = true
}

// ingestHeaders takes the upstream's own account of the window when it sends
// one — the best source available, because it is the authority.
func (l *quotaLedger) ingestHeaders(attempt modelAttempt, limitHdr, remainingHdr string, reset time.Time) {
	if l == nil {
		return
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(limitHdr), 10, 64)
	if err != nil || limit <= 0 {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windowLocked(modelScope(attempt), true, now)
	w.Limit = limit
	w.Source = "header"
	if remaining, err := strconv.ParseInt(strings.TrimSpace(remainingHdr), 10, 64); err == nil && remaining >= 0 {
		w.Used = limit - remaining
	}
	if !reset.IsZero() {
		w.ResetAt = reset
	}
	l.dirty = true
}

// QuotaSnapshot is the observable view for /health and the dashboard.
type QuotaSnapshot struct {
	Scope   string    `json:"scope"`
	Limit   int64     `json:"limit,omitempty"`
	Used    int64     `json:"used"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Source  string    `json:"source,omitempty"`
}

func (l *quotaLedger) observe() []QuotaSnapshot {
	if l == nil {
		return nil
	}
	now := time.Now()
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]QuotaSnapshot, 0, len(l.scopes))
	for scope, w := range l.scopes {
		used := w.Used
		if !w.ResetAt.After(now) {
			used = 0 // reporting a closed window's count would be a lie
		}
		out = append(out, QuotaSnapshot{Scope: scope, Limit: w.Limit, Used: used, ResetAt: w.ResetAt, Source: w.Source})
	}
	return out
}

// parseRateLimitReset reads an X-RateLimit-Reset value. Providers disagree on
// the unit — seconds since epoch, milliseconds since epoch, or seconds from now
// — so the magnitude decides, and anything unrecognisable yields the zero time
// rather than a confidently wrong window.
func parseRateLimitReset(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch {
	case n > 1e12: // milliseconds since epoch
		return time.UnixMilli(n)
	case n > 1e9: // seconds since epoch
		return time.Unix(n, 0)
	default: // a relative number of seconds
		return time.Now().Add(time.Duration(n) * time.Second)
	}
}
