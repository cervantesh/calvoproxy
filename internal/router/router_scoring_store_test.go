package router

import (
	"path/filepath"
	"testing"
	"time"
)

// storeService builds a router whose policy has one profile so knownBreakerKeys
// is deterministic, with persistence pointed at a temp file.
func storeService(t *testing.T, chain ...string) (*RouterService, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scores.json")
	t.Setenv("PROXY_SCORE_FILE", path)
	s := &RouterService{
		modelBreakers:    map[string]*modelBreakerState{},
		providerAttempts: map[providerID]int64{},
		providerBalance:  map[providerID]int64{},
		policy:           policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": chain}},
	}
	return s, path
}

// The headline of the second defect: the score map was in-memory only, so every
// restart discarded everything the proxy had learned.
func TestScoresSurviveARestart(t *testing.T) {
	s, path := storeService(t, "good", "slow")
	now := time.Now()
	s.modelBreakers["coding:good"] = &modelBreakerState{Score: 0.95, ScoreUpdatedAt: now, ScoreAttemptSeq: 40, Successes: 96}
	s.modelBreakers["coding:slow"] = &modelBreakerState{Score: 0.15, ScoreUpdatedAt: now, ScoreAttemptSeq: 41, Successes: 0}
	s.scoreAttempts.Store(41)
	s.providerAttempts[providerOpenRouter] = 17
	s.providerAttempts[providerCerebras] = 16
	s.providerAttempts[providerGroq] = 16
	s.providerBalance[providerOpenRouter] = 9
	s.providerBalance[providerCerebras] = 8
	s.providerBalance[providerGroq] = 8
	s.markScoresDirty()
	if err := s.SaveScores(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A "restart": a brand-new service reading the same file.
	restarted, _ := storeService(t, "good", "slow")
	t.Setenv("PROXY_SCORE_FILE", path)
	restarted.LoadScores()

	good := restarted.modelBreakers["coding:good"]
	slow := restarted.modelBreakers["coding:slow"]
	if good == nil || slow == nil {
		t.Fatalf("scores were not restored: good=%v slow=%v", good, slow)
	}
	if good.Score != 0.95 || good.Successes != 96 {
		t.Errorf("restored good score wrong: %+v", good)
	}
	if slow.Score != 0.15 {
		t.Errorf("restored slow score wrong: %+v", slow)
	}
	if got := restarted.scoreAttempts.Load(); got != 41 {
		t.Errorf("the evidence clock must be restored too, got %d", got)
	}
	if restarted.providerAttempts[providerOpenRouter] != 17 || restarted.providerAttempts[providerCerebras] != 16 || restarted.providerAttempts[providerGroq] != 16 {
		t.Errorf("provider consumption clock must survive restart, got %v", restarted.providerAttempts)
	}
	if restarted.providerBalance[providerOpenRouter] != 9 || restarted.providerBalance[providerCerebras] != 8 || restarted.providerBalance[providerGroq] != 8 {
		t.Errorf("provider scheduling clock must survive restart, got %v", restarted.providerBalance)
	}
	// And the whole point: the chain still knows which one to try first.
	t.Setenv("PROXY_SCORING_ENABLED", "true")
	ranked := restarted.rankAttemptsByScore([]modelAttempt{
		{Profile: "coding", Model: "slow"},
		{Profile: "coding", Model: "good"},
	})
	if ranked[0].Model != "good" {
		t.Fatalf("after a restart the chain must still prefer the proven model, got %q", ranked[0].Model)
	}
}

func TestActiveQuotaSurvivesRestartWithoutInflightReservations(t *testing.T) {
	s, path := storeService(t, "good")
	now := time.Now()
	s.quota = NewQuotaLedger()
	active := QuotaBucketKey{Provider: string(providerGroq), Scope: credentialQuotaScope("model", "secret"), ModelOrPool: "good", Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}
	expired := QuotaBucketKey{Provider: string(providerCerebras), Scope: credentialQuotaScope("model", "secret"), ModelOrPool: "good", Dimension: QuotaDimensionTokens, Window: QuotaWindowMinute}
	s.quota.Observe(active, QuotaObservation{Limit: 100, Remaining: 7, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	s.quota.Observe(expired, QuotaObservation{Limit: 1000, Remaining: 0, ResetAt: now.Add(time.Millisecond), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	if _, ok := s.quota.Reserve(active, 2, now); !ok {
		t.Fatal("could not create in-flight reservation")
	}
	s.markScoresDirty()
	if err := s.SaveScores(); err != nil {
		t.Fatalf("save: %v", err)
	}

	restarted, _ := storeService(t, "good")
	t.Setenv("PROXY_SCORE_FILE", path)
	restarted.LoadScores()
	snapshot, ok := restarted.quota.Snapshot(active, now.Add(2*time.Millisecond))
	if !ok || snapshot.Remaining != 7 || snapshot.Reserved != 0 || snapshot.Available != 7 {
		t.Fatalf("active quota was not restored without reservations: %+v ok=%v", snapshot, ok)
	}
	if _, ok := restarted.quota.Snapshot(expired, now.Add(2*time.Millisecond)); ok {
		t.Fatal("expired quota must not survive restart")
	}
}

func TestVersionOneScoreStoreStillLoads(t *testing.T) {
	s, path := storeService(t, "good")
	now := time.Now()
	if err := writeScoreFile(path, scoreStoreFile{Version: 1, SavedAt: now, Models: map[string]persistedScore{"coding:good": {Score: .7, ScoreUpdatedAt: now, Successes: 1}}}); err != nil {
		t.Fatal(err)
	}
	s.LoadScores()
	if got := s.modelBreakers["coding:good"]; got == nil || got.Score != .7 {
		t.Fatalf("version 1 store lost backward compatibility: %+v", got)
	}
}

// A file written days ago describes models as they were, not as they are.
func TestStaleStoreFileIsDiscarded(t *testing.T) {
	s, path := storeService(t, "good")
	old := time.Now().Add(-72 * time.Hour)
	if err := writeScoreFile(path, scoreStoreFile{
		Version: scoreStoreVersion,
		SavedAt: old,
		Models:  map[string]persistedScore{"coding:good": {Score: 0.1, ScoreUpdatedAt: old, Successes: 3}},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.LoadScores()
	if len(s.modelBreakers) != 0 {
		t.Fatalf("a store file older than the max age must be discarded, got %d entries", len(s.modelBreakers))
	}
}

// A model that is no longer in any chain is dropped rather than carried forever.
func TestKeysAbsentFromThePolicyAreDropped(t *testing.T) {
	s, path := storeService(t, "still-here")
	now := time.Now()
	if err := writeScoreFile(path, scoreStoreFile{
		Version: scoreStoreVersion,
		SavedAt: now,
		Models: map[string]persistedScore{
			"coding:still-here": {Score: 0.3, ScoreUpdatedAt: now, Successes: 2},
			"coding:retired":    {Score: 0.1, ScoreUpdatedAt: now, Successes: 1},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.LoadScores()
	if _, ok := s.modelBreakers["coding:retired"]; ok {
		t.Error("a key absent from the current policy must not be restored")
	}
	if _, ok := s.modelBreakers["coding:still-here"]; !ok {
		t.Error("a key still in the policy must be restored")
	}
}

// Restoring an open circuit would keep a healthy model excluded for a cooldown
// that already expired while the process was down.
func TestBreakerStateIsNotPersisted(t *testing.T) {
	s, path := storeService(t, "good")
	now := time.Now()
	s.modelBreakers["coding:good"] = &modelBreakerState{
		Score: 0.4, ScoreUpdatedAt: now, Successes: 1,
		ConsecutiveFailures: 5, OpenUntil: now.Add(time.Hour), ProbeUntil: now.Add(time.Minute),
	}
	s.markScoresDirty()
	if err := s.SaveScores(); err != nil {
		t.Fatalf("save: %v", err)
	}
	restarted, _ := storeService(t, "good")
	t.Setenv("PROXY_SCORE_FILE", path)
	restarted.LoadScores()
	st := restarted.modelBreakers["coding:good"]
	if st == nil {
		t.Fatal("score was not restored at all")
	}
	if st.ConsecutiveFailures != 0 || !st.OpenUntil.IsZero() || !st.ProbeUntil.IsZero() {
		t.Errorf("breaker state must not survive a restart: %+v", st)
	}
}

// Persistence must never scatter state into the working directory, and must be
// switchable off outright.
func TestScoreFilePathCanBeDisabled(t *testing.T) {
	t.Setenv("PROXY_SCORE_FILE", "off")
	if p := scoreFilePath(); p != "" {
		t.Fatalf("PROXY_SCORE_FILE=off must disable persistence, got %q", p)
	}
	t.Setenv("PROXY_SCORE_FILE", "")
	if p := scoreFilePath(); p != "" && !filepath.IsAbs(p) {
		t.Fatalf("the default score path must be absolute, got %q", p)
	}
}

// A file written by a future (or past) schema is ignored rather than
// half-interpreted into the live map.
func TestStoreFileFromAnotherVersionIsIgnored(t *testing.T) {
	s, path := storeService(t, "good")
	now := time.Now()
	if err := writeScoreFile(path, scoreStoreFile{
		Version: scoreStoreVersion + 1,
		SavedAt: now,
		Models:  map[string]persistedScore{"coding:good": {Score: 0.3, ScoreUpdatedAt: now, Successes: 2}},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.LoadScores()
	if len(s.modelBreakers) != 0 {
		t.Fatalf("a store file from another version must restore nothing, got %d entries", len(s.modelBreakers))
	}
}

// The file is operator-editable and survives crashes mid-write, so a value
// outside [0,1] must not escape into the ranking arithmetic.
func TestRestoredScoresAreClamped(t *testing.T) {
	s, path := storeService(t, "high", "low")
	now := time.Now()
	if err := writeScoreFile(path, scoreStoreFile{
		Version: scoreStoreVersion,
		SavedAt: now,
		Models: map[string]persistedScore{
			"coding:high": {Score: 42, ScoreUpdatedAt: now, Successes: 1},
			"coding:low":  {Score: -7, ScoreUpdatedAt: now, Successes: 1},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.LoadScores()
	if got := s.modelBreakers["coding:high"].Score; got != 1 {
		t.Errorf("an out-of-range high score must clamp to 1, got %v", got)
	}
	if got := s.modelBreakers["coding:low"].Score; got != 0 {
		t.Errorf("an out-of-range low score must clamp to 0, got %v", got)
	}
}

// SaveScores is a no-op when nothing changed, so an idle proxy doesn't rewrite
// the same bytes every flush interval.
func TestSaveIsSkippedWhenNothingChanged(t *testing.T) {
	s, path := storeService(t, "good")
	if err := s.SaveScores(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := readScoreFile(path); err == nil {
		t.Fatal("a clean map must not write a file at all")
	}
	s.markScoresDirty()
	if err := s.SaveScores(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := readScoreFile(path); err != nil {
		t.Fatalf("a dirty map must write: %v", err)
	}
}
