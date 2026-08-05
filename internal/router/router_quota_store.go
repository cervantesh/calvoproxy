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

// Quotas persist to their OWN file, deliberately not inside scores.json.
//
// The score store discards a whole file older than defaultScoreMaxAge and drops
// entries whose key is not in knownBreakerKeys(). Both rules are right for
// reliability evidence and wrong for a budget: a quota's expiry is its ResetAt,
// and its keys are bare models plus "account", which knownBreakerKeys never
// contains. Embedding it would mean three exceptions to three rules inside one
// loader, plus a flush coupled to breakerMu via snapshotScores.
//
// What IS shared is the write discipline: temp file in the same dir → rename,
// 0600 under 0700, and the same lazy 30s flusher.

const quotaStoreVersion = 1

type persistedQuota struct {
	Limit   int64     `json:"limit,omitempty"`
	Used    int64     `json:"used"`
	ResetAt time.Time `json:"reset_at"`
	Source  string    `json:"source,omitempty"`
	Daily   bool      `json:"daily"`
}

type quotaStoreFile struct {
	Version int                       `json:"version"`
	SavedAt time.Time                 `json:"saved_at"`
	Scopes  map[string]persistedQuota `json:"scopes"`
}

// quotaFilePath mirrors scoreFilePath: <user-config-dir>/calvoproxy/quotas.json,
// overridable, and "" means "no store" rather than scattering state into the
// working directory.
func quotaFilePath() string {
	raw := strings.TrimSpace(envValue("PROXY_QUOTA_FILE"))
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
	return filepath.Join(dir, "calvoproxy", "quotas.json")
}

func readQuotaFile(path string) (quotaStoreFile, error) {
	var file quotaStoreFile
	data, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, err
	}
	return file, nil
}

func writeQuotaFile(path string, file quotaStoreFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".quotas-*.tmp")
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
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Remove(path) // Windows rename won't replace
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func (l *quotaLedger) snapshot() quotaStoreFile {
	file := quotaStoreFile{Version: quotaStoreVersion, SavedAt: time.Now()}
	if l == nil {
		return file
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	file.Scopes = make(map[string]persistedQuota, len(l.scopes))
	for scope, w := range l.scopes {
		file.Scopes[scope] = persistedQuota{Limit: w.Limit, Used: w.Used, ResetAt: w.ResetAt, Source: w.Source, Daily: w.daily}
	}
	return file
}

// restore brings back live windows. A window whose reset already passed while
// the process was down comes back at zero — NOT discarded: the upstream's day
// does not restart because the proxy did, and the limit is still worth knowing.
func (l *quotaLedger) restore(file quotaStoreFile) {
	if l == nil || file.Version != quotaStoreVersion {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for scope, p := range file.Scopes {
		w := &quotaWindow{Limit: p.Limit, Used: p.Used, ResetAt: p.ResetAt, Source: p.Source, daily: p.Daily}
		if !w.ResetAt.After(now) {
			w.Used = 0
			w.ResetAt = nextReset(w.daily, now)
		}
		// Configuration outranks the file: an operator who edits a limit means it.
		if configured := l.configuredLimit(scope, w.daily); configured > 0 {
			w.Limit = configured
			w.Source = "config"
		}
		l.scopes[scope] = w
	}
}

// LoadQuotas restores the persisted ledger, if there is one.
func (s *RouterService) LoadQuotas() {
	path := quotaFilePath()
	if path == "" || s.quota == nil {
		return
	}
	file, err := readQuotaFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("[CalvoProxy] could not read the quota store", slog.String("error", err.Error()))
		}
		return
	}
	s.quota.restore(file)
}

// SaveQuotas writes the ledger out when it has changed.
func (s *RouterService) SaveQuotas() error {
	path := quotaFilePath()
	if path == "" || s.quota == nil {
		return nil
	}
	s.quota.mu.Lock()
	dirty := s.quota.dirty
	s.quota.dirty = false
	s.quota.mu.Unlock()
	if !dirty {
		return nil
	}
	if err := writeQuotaFile(path, s.quota.snapshot()); err != nil {
		s.quota.mu.Lock()
		s.quota.dirty = true // retry on the next tick
		s.quota.mu.Unlock()
		return err
	}
	return nil
}

// StartQuotaPersistence loads the ledger and starts the flusher, mirroring
// StartScorePersistence. Called by the binary, never by NewRouterService: a
// router built in a test must not read or write the operator's real files.
func (s *RouterService) StartQuotaPersistence(ctx context.Context) {
	if quotaFilePath() == "" || s.quota == nil {
		return
	}
	s.LoadQuotas()
	s.persistQuotas.Store(true)
	go func() {
		ticker := time.NewTicker(defaultScoreFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.SaveQuotas(); err != nil {
					slog.Warn("[CalvoProxy] could not persist quotas", slog.String("error", err.Error()))
				}
			}
		}
	}()
}
