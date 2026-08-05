package router

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLedger(t *testing.T) *quotaLedger {
	t.Helper()
	return newQuotaLedger(map[string]quotaLimit{
		"model:org/alpha:free": {RPD: 10},
		"account":              {RPD: 100},
	})
}

// Invariant 1: quota is keyed by the BARE model, never by breakerKey. breakerKey
// is profile+":"+model, and the same slug lives in several profiles today — two
// partial counters of the same OpenRouter pocket would each see half the traffic
// and never detect exhaustion.
func TestQuota_KeyedByBareModelNotProfile(t *testing.T) {
	l := testLedger(t)

	l.record(modelAttempt{Profile: "coding", Model: "org/alpha:free"})
	l.record(modelAttempt{Profile: "bulk", Model: "org/alpha:free"})
	l.record(modelAttempt{Profile: "reasoning", Model: "org/alpha:free"})

	if used := l.used("model:org/alpha:free"); used != 3 {
		t.Errorf("used = %d, want 3: the same model under three profiles is one pocket", used)
	}
}

// Invariant 2: the account scope counts every request whatever the model. The
// free tier's dominant limit is per account, so a model-only ledger would model
// the wrong thing.
func TestQuota_AccountScopeCountsEveryModel(t *testing.T) {
	l := testLedger(t)

	l.record(modelAttempt{Profile: "coding", Model: "org/alpha:free"})
	l.record(modelAttempt{Profile: "coding", Model: "org/beta:free"})

	if used := l.used("account"); used != 2 {
		t.Errorf("account used = %d, want 2", used)
	}
}

// Invariant 3: a window whose reset has passed goes back to zero — it is not
// discarded. Discarding would lose the limit along with the count.
func TestQuota_ExpiredWindowResetsToZeroKeepingLimit(t *testing.T) {
	l := testLedger(t)
	l.record(modelAttempt{Model: "org/alpha:free"})

	l.mu.Lock()
	l.scopes["model:org/alpha:free"].ResetAt = time.Now().Add(-time.Minute)
	l.mu.Unlock()

	if used := l.used("model:org/alpha:free"); used != 0 {
		t.Errorf("used = %d after the window expired, want 0", used)
	}
	if limit := l.limit("model:org/alpha:free"); limit != 10 {
		t.Errorf("limit = %d, want the configured 10 to survive the roll", limit)
	}
}

// Invariant 6: with no known limit there is no gate. Inventing a ceiling would
// be worse than having none — a 429 says "not now", not "how many fit".
func TestQuota_UnknownLimitMeansFullHeadroom(t *testing.T) {
	// No limits at all, model OR account: headroom is the minimum across every
	// window that applies, and the account budget legitimately constrains a model
	// that has no ceiling of its own. Isolating the case means configuring none.
	l := newQuotaLedger(nil)
	for i := 0; i < 50; i++ {
		l.record(modelAttempt{Model: "org/sin-limite:free"})
	}
	if h := l.headroom(modelAttempt{Model: "org/sin-limite:free"}); h != 1 {
		t.Errorf("headroom = %v, want 1 with no configured limit", h)
	}
}

// The account budget constrains a model that has no ceiling of its own — that is
// the whole point of tracking it as a separate scope.
func TestQuota_AccountLimitConstrainsUncappedModel(t *testing.T) {
	l := newQuotaLedger(map[string]quotaLimit{"account": {RPD: 100}})
	for i := 0; i < 50; i++ {
		l.record(modelAttempt{Model: "org/sin-limite:free"})
	}
	if h := l.headroom(modelAttempt{Model: "org/sin-limite:free"}); h != 0.5 {
		t.Errorf("headroom = %v, want 0.5 from the account window alone", h)
	}
}

// Headroom falls as the window fills, which is what turns "degrade before
// exhausting" into a ranking decision instead of an exclusion.
func TestQuota_HeadroomFallsAsWindowFills(t *testing.T) {
	l := testLedger(t)
	attempt := modelAttempt{Model: "org/alpha:free"}

	full := l.headroom(attempt)
	for i := 0; i < 5; i++ {
		l.record(attempt)
	}
	half := l.headroom(attempt)
	for i := 0; i < 5; i++ {
		l.record(attempt)
	}
	empty := l.headroom(attempt)

	if full != 1 {
		t.Errorf("fresh headroom = %v, want 1", full)
	}
	if !(half < full && half > 0) {
		t.Errorf("half-used headroom = %v, want between 0 and %v", half, full)
	}
	if empty != 0 {
		t.Errorf("exhausted headroom = %v, want 0", empty)
	}
}

// Invariant 7: hard exclusion only under PROXY_QUOTA_HARD_SKIP. The default is
// soft because a hard skip widens the "all models cooling" surface on the
// strength of limits that may have been learned, and therefore may be wrong.
func TestQuota_HardSkipIsOptIn(t *testing.T) {
	l := testLedger(t)
	attempt := modelAttempt{Model: "org/alpha:free"}
	for i := 0; i < 10; i++ {
		l.record(attempt)
	}

	t.Setenv("PROXY_QUOTA_HARD_SKIP", "")
	if l.exhausted(attempt) {
		t.Error("exhausted() = true by default; soft degradation must be the default")
	}

	t.Setenv("PROXY_QUOTA_HARD_SKIP", "true")
	if !l.exhausted(attempt) {
		t.Error("exhausted() = false with PROXY_QUOTA_HARD_SKIP=true")
	}
}

// Invariant 10: learning from a 429 sets when to come back, never how many fit.
func TestQuota_LearnsResetButNeverInventsLimit(t *testing.T) {
	l := newQuotaLedger(nil)
	attempt := modelAttempt{Model: "org/gamma:free"}

	l.learnFrom429(attempt, 90*time.Second)

	if limit := l.limit("model:org/gamma:free"); limit != 0 {
		t.Errorf("limit = %d, want 0: a 429 says 'not now', not 'how many'", limit)
	}
	l.mu.RLock()
	reset := l.scopes["model:org/gamma:free"].ResetAt
	l.mu.RUnlock()
	if time.Until(reset) < 30*time.Second {
		t.Errorf("ResetAt = %v, want roughly 90s out", reset)
	}
	// With no limit there is still no gate: a learned reset must not silently
	// become a hard exclusion.
	if h := l.headroom(attempt); h != 1 {
		t.Errorf("headroom = %v after learning, want 1 with no known limit", h)
	}
}

// Upstream rate-limit headers are the best source when they arrive.
func TestQuota_IngestsRateLimitHeaders(t *testing.T) {
	l := newQuotaLedger(nil)
	attempt := modelAttempt{Model: "org/delta:free"}

	l.ingestHeaders(attempt, "50", "12", time.Now().Add(time.Hour))

	if got := l.limit("model:org/delta:free"); got != 50 {
		t.Errorf("limit = %d, want 50 from the header", got)
	}
	if got := l.used("model:org/delta:free"); got != 38 {
		t.Errorf("used = %d, want 38 (limit 50 - remaining 12)", got)
	}
}

// Invariant 4: quotas survive a restart, and a window that already rolled while
// the process was down loads as zero. The upstream's day does not restart
// because the proxy did.
func TestQuota_PersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotas.json")

	l := testLedger(t)
	l.record(modelAttempt{Model: "org/alpha:free"})
	l.record(modelAttempt{Model: "org/alpha:free"})
	// A scope whose window already closed.
	l.mu.Lock()
	l.scopes["account"].ResetAt = time.Now().Add(-time.Hour)
	l.mu.Unlock()

	if err := writeQuotaFile(path, l.snapshot()); err != nil {
		t.Fatalf("write: %v", err)
	}

	restored := newQuotaLedger(map[string]quotaLimit{"model:org/alpha:free": {RPD: 10}, "account": {RPD: 100}})
	file, err := readQuotaFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	restored.restore(file)

	if used := restored.used("model:org/alpha:free"); used != 2 {
		t.Errorf("restored used = %d, want 2: a live window must survive a restart", used)
	}
	if used := restored.used("account"); used != 0 {
		t.Errorf("restored account used = %d, want 0: its window had already rolled", used)
	}
}

// Invariant 9: Health() holds breakerMu and reaches the ledger. The lock order
// is breakerMu → quotaMu and the ledger must never call back into the breaker;
// this exercises both directions concurrently under -race.
func TestQuota_NoDeadlockUnderConcurrentHealthAndTraffic(t *testing.T) {
	svc := &RouterService{modelBreakers: map[string]*modelBreakerState{}, quota: testLedger(t)}
	svc.policy = policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"org/alpha:free", "org/beta:free"}},
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = svc.Health()
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				svc.quota.record(modelAttempt{Profile: "coding", Model: "org/alpha:free"})
				_ = svc.quota.headroom(modelAttempt{Profile: "coding", Model: "org/alpha:free"})
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

// A nil ledger is a no-op everywhere: quota is a feature that can be absent, and
// every call site must tolerate that without a nil check of its own.
func TestQuota_NilLedgerIsNoOp(t *testing.T) {
	var l *quotaLedger
	l.record(modelAttempt{Model: "x"})
	l.learnFrom429(modelAttempt{Model: "x"}, time.Second)
	l.ingestHeaders(modelAttempt{Model: "x"}, "1", "1", time.Now())
	if h := l.headroom(modelAttempt{Model: "x"}); h != 1 {
		t.Errorf("nil ledger headroom = %v, want 1", h)
	}
	if l.exhausted(modelAttempt{Model: "x"}) {
		t.Error("nil ledger must never exhaust")
	}
	if l.snapshot().Scopes != nil {
		t.Error("nil ledger snapshot should be empty")
	}
}

// Configuration comes from env, not from model-policy.json: that file's shape is
// owned by the vendored policy package.
func TestQuota_ParsesLimitsFromEnv(t *testing.T) {
	t.Setenv("PROXY_QUOTA_LIMITS_JSON", `{"model:org/alpha:free":{"rpd":25},"account":{"rpd":500,"rpm":20}}`)

	limits := quotaLimitsFromEnv()
	if limits["model:org/alpha:free"].RPD != 25 {
		t.Errorf("model rpd = %d, want 25", limits["model:org/alpha:free"].RPD)
	}
	if limits["account"].RPM != 20 {
		t.Errorf("account rpm = %d, want 20", limits["account"].RPM)
	}
}

// Malformed configuration must not take the proxy down: quota is an
// optimisation, and a typo in an env var is not a reason to refuse traffic.
func TestQuota_MalformedEnvIsIgnored(t *testing.T) {
	t.Setenv("PROXY_QUOTA_LIMITS_JSON", `{not json`)
	if limits := quotaLimitsFromEnv(); len(limits) != 0 {
		t.Errorf("malformed env produced %v, want empty", limits)
	}
}

// Invariant 5: soft degradation reorders the chain without touching the
// persisted score. The score measures reliability; a model that is merely
// popular must not come back looking broken.
func TestQuota_SoftDegradationLeavesScoreUntouched(t *testing.T) {
	svc := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	svc.policy = policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": {"org/alpha:free", "org/beta:free"}}}
	svc.quota = newQuotaLedger(map[string]quotaLimit{"model:org/alpha:free": {RPD: 4}})

	alpha := modelAttempt{Profile: "coding", Model: "org/alpha:free"}
	beta := modelAttempt{Profile: "coding", Model: "org/beta:free"}
	// Give both a real, equal score so ranking has something to reorder.
	svc.recordSuccess(alpha)
	svc.recordSuccess(beta)
	env := svc.scoreEnv()
	before := svc.scoreForAttempt(alpha, env)

	// Spend alpha's window down to nothing.
	for i := 0; i < 4; i++ {
		svc.quota.record(alpha)
	}

	ranked := svc.rankAttemptsByScore([]modelAttempt{alpha, beta})
	if ranked[0].Model != "org/beta:free" {
		t.Errorf("ranked first = %s, want the model with headroom left", ranked[0].Model)
	}
	if after := svc.scoreForAttempt(alpha, svc.scoreEnv()); after != before {
		t.Errorf("persisted score moved from %v to %v; quota must not touch it", before, after)
	}
}

// Invariant 7b: with hard skip off, an exhausted model is still eligible — it
// just ranks last. Soft degradation must never silently become exclusion.
func TestQuota_ExhaustedModelStaysEligibleWhenSoft(t *testing.T) {
	t.Setenv("PROXY_QUOTA_HARD_SKIP", "")
	svc := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	svc.policy = policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": {"org/alpha:free"}}}
	svc.quota = newQuotaLedger(map[string]quotaLimit{"model:org/alpha:free": {RPD: 1}})

	alpha := modelAttempt{Profile: "coding", Model: "org/alpha:free"}
	svc.quota.record(alpha)

	if got := svc.filterAvailableAttempts([]modelAttempt{alpha}); len(got) != 1 {
		t.Errorf("exhausted model was excluded with hard skip off: %v", got)
	}

	t.Setenv("PROXY_QUOTA_HARD_SKIP", "true")
	if got := svc.filterAvailableAttempts([]modelAttempt{alpha}); len(got) != 0 {
		t.Errorf("exhausted model survived hard skip: %v", got)
	}
}

// A quota-emptied chain still tells the client when to come back.
func TestQuota_RetryAfterCoversQuotaOnlyExhaustion(t *testing.T) {
	svc := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	svc.policy = policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": {"org/alpha:free"}}}
	svc.quota = newQuotaLedger(map[string]quotaLimit{"model:org/alpha:free": {RPM: 1}})

	alpha := modelAttempt{Profile: "coding", Model: "org/alpha:free"}
	svc.quota.record(alpha)

	if wait := svc.retryAfterForAttempts([]modelAttempt{alpha}); wait <= 0 {
		t.Error("Retry-After = 0 for a chain emptied by quota; the client is told nothing")
	}
}

// The trace must not report a spent budget as an open circuit: "broken now" and
// "out of allowance until midnight" call for different actions.
func TestQuota_TraceSeparatesQuotaFromBreaker(t *testing.T) {
	tr := &routeTrace{Profile: "coding"}
	tr.recordQuotaExclusions(2)
	tr.recordChain(4, 4, 1, nil)

	if tr.ExcludedByQuota != 2 {
		t.Errorf("ExcludedByQuota = %d, want 2", tr.ExcludedByQuota)
	}
	if tr.ExcludedByBreaker != 1 {
		t.Errorf("ExcludedByBreaker = %d, want 1 (4 planned - 1 eligible - 2 quota)", tr.ExcludedByBreaker)
	}
	header := tr.header()
	if !strings.Contains(header, "q=2") {
		t.Errorf("header does not report the quota exclusions: %s", header)
	}
	if !strings.Contains(header, "brk=1") {
		t.Errorf("header lost the breaker exclusions: %s", header)
	}
}
