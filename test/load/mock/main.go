package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Mock OpenRouter upstream for stress testing. Returns 200 JSON (or SSE when
// stream:true), with a configurable fraction of 429/500 to drive the proxy's
// circuit breaker + scoring, and a little random latency.
func main() {
	addr := getenv("MOCK_ADDR", "127.0.0.1:29900")
	failPct := atoiEnv("MOCK_FAIL_PCT", 15) // % of requests that 429/500
	maxLatMs := atoiEnv("MOCK_MAX_LATENCY_MS", 25)

	var served, failed atomic.Int64
	rng := rand.New(rand.NewSource(1)) // deterministic-ish; concurrency still races reads harmlessly

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		served.Add(1)
		if maxLatMs > 0 {
			time.Sleep(time.Duration(rng.Intn(maxLatMs)) * time.Millisecond)
		}
		// Inject failures to exercise breaker/scoring/fallback.
		if rng.Intn(100) < failPct {
			failed.Add(1)
			if rng.Intn(2) == 0 {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":{"message":"rate limited","code":429}}`)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"message":"upstream boom","code":500}}`)
			}
			return
		}
		if strings.Contains(string(body), `"stream":true`) || strings.Contains(string(body), `"stream": true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for i := 0; i < 6; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(3 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"mock","object":"chat.completion","model":"mock/model:free","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"OK"}}],"usage":{"total_tokens":5}}`)
	})

	go func() {
		for range time.Tick(2 * time.Second) {
			fmt.Fprintf(os.Stderr, "[mock] served=%d injected_failures=%d\n", served.Load(), failed.Load())
		}
	}()

	fmt.Fprintf(os.Stderr, "[mock] listening on %s (fail=%d%%, maxLat=%dms)\n", addr, failPct, maxLatMs)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "mock error:", err)
		os.Exit(1)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func atoiEnv(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return d
}
