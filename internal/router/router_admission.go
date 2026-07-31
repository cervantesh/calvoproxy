package router

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// admissionControl caps the number of concurrent in-flight proxy requests so a
// burst can't blow past upstream rate limits (or exhaust local resources) and
// cascade the whole fallback chain into 503s. Disabled by default
// (PROXY_MAX_CONCURRENT unset/0) — an unbounded proxy, unchanged behaviour.
type admissionControl struct {
	sem     chan struct{} // nil ⇒ disabled
	timeout time.Duration
}

func newAdmissionControl() *admissionControl {
	max := envInt("PROXY_MAX_CONCURRENT", 0)
	if max <= 0 {
		return &admissionControl{} // disabled
	}
	return &admissionControl{
		sem:     make(chan struct{}, max),
		timeout: envDurationSeconds("PROXY_ADMISSION_TIMEOUT_SECONDS", 5*time.Second),
	}
}

// acquire admits a request or blocks until a slot frees, the caller's context is
// done, or the admission timeout elapses. It returns a release func (always safe
// to call) and ok=false when the request was NOT admitted. When disabled it
// admits immediately.
func (a *admissionControl) acquire(ctx context.Context) (func(), bool) {
	if a == nil || a.sem == nil {
		return func() {}, true
	}
	timer := time.NewTimer(a.timeout)
	defer timer.Stop()
	select {
	case a.sem <- struct{}{}:
		return func() { <-a.sem }, true
	case <-ctx.Done():
		return func() {}, false
	case <-timer.C:
		return func() {}, false
	}
}

// enabled reports whether admission control is active.
func (a *admissionControl) enabled() bool { return a != nil && a.sem != nil }

// retryAfterSeconds is the Retry-After hint to give a rejected caller: the
// admission wait budget, floored at 1 second.
func (a *admissionControl) retryAfterSeconds() int {
	if a == nil || a.timeout <= time.Second {
		return 1
	}
	return int(a.timeout / time.Second)
}

// parseRetryAfter interprets an HTTP Retry-After header value, which is either a
// delay in seconds or an HTTP-date. Returns 0 when absent/unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
