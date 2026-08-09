package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cervantesh/calvoproxy/internal/router"
)

// TestLoad_NoDeadlockUnderConcurrency is a CI regression gate for the whole
// stack under concurrent load. It drives a real httptest server wrapping the
// proxy mux (so the upstream connection pool is genuinely exercised) against a
// mock upstream that injects failures, while also hammering /health. It asserts
// the invariants that matter — the proxy must not deadlock or crash, must stay
// responsive on /health, must not leak client transport errors, and must serve
// the overwhelming majority of requests via fallback.
//
// Scale up for a heavy benchmark with env overrides, e.g.:
//
//	LOAD_N=25000 LOAD_C=200 go test ./cmd -run TestLoad -v
func TestLoad_NoDeadlockUnderConcurrency(t *testing.T) {
	n := envIntTest("LOAD_N", 1500)
	conc := envIntTest("LOAD_C", 40)
	failPct := envIntTest("LOAD_FAIL_PCT", 10)

	bindHost = "127.0.0.1"
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	// This is a proxy concurrency/deadlock test against a local mock, not an
	// upstream free-tier quota test. Keep the quota guard out of its artificial
	// 1,500-request burst while the dedicated router tests exercise RPM/RPD.
	t.Setenv("PROXY_OPENROUTER_FREE_RPM", "200000")
	t.Setenv("PROXY_OPENROUTER_FREE_RPD", "200000")

	// Mock upstream: mostly 200 (JSON or SSE), failPct% model-local 500. Quota
	// behavior has dedicated deterministic tests; random 429s would correctly
	// cool all four models and turn this into a quota test instead of a lock test.
	var rngMu sync.Mutex
	rng := rand.New(rand.NewSource(1))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rngMu.Lock()
		roll := rng.Intn(100)
		rngMu.Unlock()
		if roll < failPct {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"synthetic model failure"}}`)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for i := 0; i < 4; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
				if fl != nil {
					fl.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)
	t.Setenv("PROXY_BREAKER_COOLDOWN_SECONDS", "1")

	proxy := httptest.NewServer(newMux(router.NewRouterService(), nil))
	defer proxy.Close()

	tr := &http.Transport{MaxIdleConns: conc + 16, MaxIdleConnsPerHost: conc + 16, IdleConnTimeout: 30 * time.Second}
	client := &http.Client{Timeout: 30 * time.Second, Transport: tr}

	var (
		transportErrs atomic.Int64
		success       atomic.Int64
		errStatus     atomic.Int64
		healthBad     atomic.Int64
	)

	// Concurrent /health pollers to stress the read-lock path against breaker
	// write-lock churn (the recursive-RLock deadlock regression guard).
	stopHealth := make(chan struct{})
	var hwg sync.WaitGroup
	for i := 0; i < 3; i++ {
		hwg.Add(1)
		go func() {
			defer hwg.Done()
			for {
				select {
				case <-stopHealth:
					return
				default:
					time.Sleep(5 * time.Millisecond) // avoid a busy-spin CPU hog
					r, err := client.Get(proxy.URL + "/health")
					if err != nil {
						healthBad.Add(1)
						continue
					}
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
					if r.StatusCode != 200 {
						healthBad.Add(1)
					}
				}
			}
		}()
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wrng := rand.New(rand.NewSource(int64(time.Now().UnixNano())))
			for range jobs {
				stream := wrng.Intn(100) < 25
				body := `{"model":"auto","messages":[{"role":"user","content":"go"}]}`
				if stream {
					body = `{"model":"auto","stream":true,"messages":[{"role":"user","content":"go"}]}`
				}
				req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/coding/chat/completions", bytes.NewBufferString(body))
				req.Header.Set("Authorization", "Bearer dummy")
				resp, err := client.Do(req)
				if err != nil {
					transportErrs.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					success.Add(1)
				} else {
					errStatus.Add(1)
				}
			}
		}()
	}

	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)

	// Deadlock failsafe scaled to the workload (heavy manual runs raise LOAD_N):
	// signal completion on a channel and fail from the test goroutine — no panic,
	// so t cleanup still runs and a legitimately long run isn't killed.
	budget := time.Duration(n)*2*time.Millisecond + 30*time.Second
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(budget):
		t.Fatalf("load test did not finish within %v (deadlock?): %d/%d completed",
			budget, success.Load()+errStatus.Load()+transportErrs.Load(), n)
	}
	close(stopHealth)
	hwg.Wait()

	total := success.Load() + errStatus.Load() + transportErrs.Load()
	t.Logf("load: n=%d conc=%d success=%d errStatus=%d transportErrs=%d healthBad=%d",
		n, conc, success.Load(), errStatus.Load(), transportErrs.Load(), healthBad.Load())

	if total != int64(n) {
		t.Fatalf("accounting mismatch: total=%d want=%d", total, n)
	}
	if transportErrs.Load() != 0 {
		t.Fatalf("client transport errors under load: %d (connection pooling / reset regression?)", transportErrs.Load())
	}
	if healthBad.Load() != 0 {
		t.Fatalf("/health became unresponsive under load: %d bad polls (locking regression?)", healthBad.Load())
	}
	// With ~failPct upstream failures and a 4-model fallback chain, the vast
	// majority must still succeed.
	if rate := float64(success.Load()) / float64(n); rate < 0.90 {
		t.Fatalf("success rate too low under load: %.3f", rate)
	}
}

func envIntTest(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
