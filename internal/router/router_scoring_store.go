package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Score persistence.
//
// The score map used to live only in memory, created fresh by NewRouterService,
// so every restart threw away everything the proxy had learned about its models
// and the next burst of traffic re-paid the whole discovery cost. Installing a
// new build wiped it; so did a crash, a config reload that restarted the
// process, or a laptop going to sleep.
//
// What is persisted is deliberately narrow: the reliability score, when it was
// last updated (on both clocks), and the success count. Breaker state —
// OpenUntil, ProbeUntil, ConsecutiveFailures — is NOT persisted. A cooldown is a
// short-lived statement about right now ("this model is rate-limited for the
// next 60s"), and a restart is a perfectly good reason to re-probe; restoring an
// open circuit would keep a healthy model excluded for a window that had already
// expired while the process was down.
const (
	scoreStoreVersion    = 2
	scoreStoreMinVersion = 1
	// defaultScoreMaxAge discards a whole store file older than this. Scores are
	// evidence about models, and a week-old file is evidence about models as they
	// were a week ago — free-tier slugs get retired and re-provisioned on exactly
	// that timescale. Override with PROXY_SCORE_MAX_AGE_SECONDS.
	defaultScoreMaxAge = 24 * time.Hour
	// defaultScoreFlushInterval is how often the background flusher writes the
	// map out, if anything changed. Deliberately lazy: losing the last few
	// seconds of scoring on a hard kill costs a handful of attempts, whereas
	// writing on every outcome would put a file write on the request path.
	defaultScoreFlushInterval = 30 * time.Second
)

// persistedScore is one model's durable reliability record.
type persistedScore struct {
	Score           float64   `json:"score"`
	ScoreUpdatedAt  time.Time `json:"score_updated_at"`
	ScoreAttemptSeq int64     `json:"score_attempt_seq,omitempty"`
	Successes       int       `json:"successes,omitempty"`
}

// scoreStoreFile is the on-disk shape.
type scoreStoreFile struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
	// AttemptSeq is the proxy-wide scored-attempt counter at save time. It has to
	// be restored alongside the per-model sequence numbers or every restored
	// score would read as "no new evidence since" forever and never decay.
	AttemptSeq int64                     `json:"attempt_seq"`
	Models     map[string]persistedScore `json:"models"`
	// ProviderAttempts preserves the fair-consumption clock across restarts.
	// Optional for backward compatibility with existing version-1 files.
	ProviderAttempts map[string]int64 `json:"provider_attempts,omitempty"`
	ProviderBalance  map[string]int64 `json:"provider_balance,omitempty"`
	Quota            []persistedQuota `json:"quota,omitempty"`
}

type persistedQuota struct {
	Provider    string          `json:"provider"`
	Scope       string          `json:"scope"`
	ModelOrPool string          `json:"model_or_pool"`
	Dimension   QuotaDimension  `json:"dimension"`
	Window      QuotaWindow     `json:"window"`
	Limit       int64           `json:"limit"`
	Remaining   int64           `json:"remaining"`
	ResetAt     time.Time       `json:"reset_at,omitempty"`
	ObservedAt  time.Time       `json:"observed_at,omitempty"`
	Source      QuotaSource     `json:"source"`
	Confidence  QuotaConfidence `json:"confidence"`
}

// scoreFilePath is where the score map is persisted. Defaults to
// <user-config-dir>/calvoproxy/scores.json, next to the login key. Override with
// PROXY_SCORE_FILE; set it to "off" (or "-") to disable persistence entirely.
// Returns "" when no safe location can be determined, and callers treat "" as
// "no store" rather than scattering state into the working directory.
func scoreFilePath() string {
	raw := strings.TrimSpace(envValue("PROXY_SCORE_FILE"))
	switch strings.ToLower(raw) {
	case "off", "-", "none", "disabled":
		return ""
	case "":
	default:
		return raw
	}
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, "calvoproxy", "scores.json")
}

func scoreMaxAge() time.Duration {
	return envDurationSeconds("PROXY_SCORE_MAX_AGE_SECONDS", defaultScoreMaxAge)
}

// markScoresDirty flags that the in-memory map has diverged from disk. Cheap and
// lock-free; the flusher clears it.
func (s *RouterService) markScoresDirty() { s.scoresDirty.Store(true) }

// snapshotScores copies the durable fields out of the live map under the read
// lock. States that have never been scored are skipped — persisting them would
// only record "we have seen nothing", which is also what their absence says.
func (s *RouterService) snapshotScores() scoreStoreFile {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	out := scoreStoreFile{
		Version:          scoreStoreVersion,
		SavedAt:          time.Now(),
		AttemptSeq:       s.scoreAttempts.Load(),
		Models:           make(map[string]persistedScore, len(s.modelBreakers)),
		ProviderAttempts: make(map[string]int64, len(s.providerAttempts)),
		ProviderBalance:  make(map[string]int64, len(s.providerBalance)),
	}
	for key, state := range s.modelBreakers {
		if state == nil || state.ScoreUpdatedAt.IsZero() {
			continue
		}
		out.Models[key] = persistedScore{
			Score:           state.Score,
			ScoreUpdatedAt:  state.ScoreUpdatedAt,
			ScoreAttemptSeq: state.ScoreAttemptSeq,
			Successes:       state.Successes,
		}
	}
	for provider, attempts := range s.providerAttempts {
		if attempts > 0 {
			out.ProviderAttempts[string(provider)] = attempts
		}
	}
	for provider, balance := range s.providerBalance {
		if balance > 0 {
			out.ProviderBalance[string(provider)] = balance
		}
	}
	if ledger := s.quotaLedger(); ledger != nil {
		now := time.Now()
		for _, key := range ledger.Keys(now) {
			snapshot, ok := ledger.Snapshot(key, now)
			if !ok {
				continue
			}
			out.Quota = append(out.Quota, persistedQuota{
				Provider: key.Provider, Scope: key.Scope, ModelOrPool: key.ModelOrPool,
				Dimension: key.Dimension, Window: key.Window, Limit: snapshot.Limit,
				Remaining: snapshot.Remaining, ResetAt: snapshot.ResetAt,
				ObservedAt: snapshot.ObservedAt, Source: snapshot.Source,
				Confidence: snapshot.Confidence,
			})
		}
	}
	return out
}

// restoreScores merges a loaded file into the live map. `known` is the set of
// breaker keys the CURRENT policy can produce; entries outside it are dropped.
//
// Dropping is the deliberate answer to "what happens when the policy changes and
// a model key disappears": an edited chain (or a retired free slug) is exactly
// the signal that the operator changed the model set, and a key no longer in any
// chain can only ever be reached by an explicit pin — where the chain is one
// model long and the score orders nothing. Keeping such keys would grow the file
// without bound across every policy revision the install has ever seen.
//
// Entries whose evidence is older than maxAge are dropped for the same reason
// the whole file is: they describe models as they were, not as they are.
func (s *RouterService) restoreScores(file scoreStoreFile, known map[string]struct{}, now time.Time, maxAge time.Duration) int {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.providerAttempts == nil {
		s.providerAttempts = make(map[providerID]int64)
	}
	if s.providerBalance == nil {
		s.providerBalance = make(map[providerID]int64)
	}
	ledger := s.quotaLedger()
	for _, persisted := range file.Quota {
		provider := providerID(strings.ToLower(strings.TrimSpace(persisted.Provider)))
		if _, supported := supportedDirectProviders[provider]; !supported || (!persisted.ResetAt.IsZero() && !persisted.ResetAt.After(now)) {
			continue
		}
		key := QuotaBucketKey{Provider: string(provider), Scope: persisted.Scope, ModelOrPool: persisted.ModelOrPool, Dimension: persisted.Dimension, Window: persisted.Window}
		ledger.Observe(key, QuotaObservation{
			Limit: persisted.Limit, Remaining: persisted.Remaining, ResetAt: persisted.ResetAt,
			ObservedAt: persisted.ObservedAt, Source: persisted.Source, Confidence: persisted.Confidence,
		}, now)
	}
	for rawProvider, attempts := range file.ProviderAttempts {
		provider := providerID(strings.ToLower(strings.TrimSpace(rawProvider)))
		if _, supported := supportedDirectProviders[provider]; supported && attempts >= 0 {
			s.providerAttempts[provider] = attempts
		}
	}
	for rawProvider, balance := range file.ProviderBalance {
		provider := providerID(strings.ToLower(strings.TrimSpace(rawProvider)))
		if _, supported := supportedDirectProviders[provider]; supported && balance >= 0 {
			s.providerBalance[provider] = balance
		}
	}
	restored := 0
	for key, rec := range file.Models {
		if _, ok := known[key]; !ok {
			continue
		}
		if rec.ScoreUpdatedAt.IsZero() {
			continue
		}
		if maxAge > 0 && now.Sub(rec.ScoreUpdatedAt) > maxAge {
			continue
		}
		state := s.modelBreakers[key]
		if state == nil {
			state = &modelBreakerState{}
			s.modelBreakers[key] = state
		}
		state.Score = clamp01(rec.Score)
		state.ScoreUpdatedAt = rec.ScoreUpdatedAt
		state.ScoreAttemptSeq = rec.ScoreAttemptSeq
		state.Successes = rec.Successes
		restored++
	}
	if restored > 0 && file.AttemptSeq > s.scoreAttempts.Load() {
		// Restore the attempt clock too, or the per-model sequence numbers we just
		// loaded would all sit in the future and no restored score could ever decay.
		s.scoreAttempts.Store(file.AttemptSeq)
	}
	return restored
}

func readScoreFile(path string) (scoreStoreFile, error) {
	var file scoreStoreFile
	data, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, err
	}
	return file, nil
}

// writeScoreFile writes atomically (temp in the same dir → rename) with 0600
// perms under a 0700 dir, mirroring how the login key is stored.
func writeScoreFile(path string, file scoreStoreFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".scores-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := atomicReplaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// knownBreakerKeys is the set of keys the current policy can produce.
func (s *RouterService) knownBreakerKeys() map[string]struct{} {
	known := make(map[string]struct{})
	for _, attempt := range s.allKnownAttempts() {
		known[s.breakerKey(attempt)] = struct{}{}
	}
	return known
}

// LoadScores restores persisted reliability scores. Best-effort: a missing,
// unreadable, corrupt, or stale file just means the proxy starts optimistic,
// which is the pre-persistence behaviour.
func (s *RouterService) LoadScores() {
	path := scoreFilePath()
	if path == "" {
		return
	}
	file, err := readScoreFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("[CalvoProxy] could not read score store", slog.String("path", path), slog.String("error", err.Error()))
		}
		return
	}
	if file.Version < scoreStoreMinVersion || file.Version > scoreStoreVersion {
		slog.Info("[CalvoProxy] ignoring score store from a different version", slog.Int("file_version", file.Version))
		return
	}
	now := time.Now()
	maxAge := scoreMaxAge()
	if maxAge > 0 && !file.SavedAt.IsZero() && now.Sub(file.SavedAt) > maxAge {
		slog.Info("[CalvoProxy] discarding stale score store", slog.String("saved_at", file.SavedAt.Format(time.RFC3339)))
		return
	}
	restored := s.restoreScores(file, s.knownBreakerKeys(), now, maxAge)
	slog.Info("[CalvoProxy] 📈 restored model scores", slog.Int("models", restored), slog.Int("in_file", len(file.Models)))
}

// SaveScores writes the current score map out. No-op when persistence is
// disabled or nothing has changed since the last write.
func (s *RouterService) SaveScores() error {
	path := scoreFilePath()
	if path == "" {
		return nil
	}
	if !s.scoresDirty.Swap(false) {
		return nil
	}
	if err := writeScoreFile(path, s.snapshotScores()); err != nil {
		s.scoresDirty.Store(true) // retry on the next tick
		return err
	}
	return nil
}

// StartScorePersistence loads the persisted scores and starts the background
// flusher. Called by the binary, not by NewRouterService: constructing a router
// in a test must not read or write the operator's real score file.
func (s *RouterService) StartScorePersistence(ctx context.Context) {
	if scoreFilePath() == "" {
		return
	}
	s.LoadScores()
	s.persistScores.Store(true)
	go func() {
		ticker := time.NewTicker(defaultScoreFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.SaveScores(); err != nil {
					slog.Warn("[CalvoProxy] could not persist model scores", slog.String("error", err.Error()))
				}
			}
		}
	}()
}
