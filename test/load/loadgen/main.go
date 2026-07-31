package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Load generator for the CalvoProxy stress test. Fires N requests across C
// workers (a mix of streaming and non-streaming) at the proxy, while extra
// goroutines hammer /health concurrently — the exact contention that the
// breaker/Health locking fix has to survive. Reports throughput, latency
// percentiles and the status-code distribution.
func main() {
	base := getenv("LOAD_URL", "http://127.0.0.1:28080")
	conc := atoiEnv("LOAD_C", 200)
	total := atoiEnv("LOAD_N", 20000)
	streamPct := atoiEnv("LOAD_STREAM_PCT", 25)
	healthPollers := atoiEnv("LOAD_HEALTH_POLLERS", 8)

	// Pooled transport so the client reuses connections instead of dialing a new
	// one per request (which under high concurrency exhausts ephemeral ports and
	// OS threads on the CLIENT — a load-tool limit, not the proxy's).
	tr := &http.Transport{
		MaxIdleConns:        conc + 32,
		MaxIdleConnsPerHost: conc + 32,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Timeout: 90 * time.Second, Transport: tr}

	var (
		done      atomic.Int64
		statusMu  sync.Mutex
		statusMap = map[int]int64{}
		errCount  atomic.Int64
		latMu     sync.Mutex
		latencies = make([]time.Duration, 0, total)
		healthOK  atomic.Int64
		healthBad atomic.Int64
	)

	jobs := make(chan int, conc*2)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for range jobs {
			stream := rng.Intn(100) < streamPct
			var body string
			if stream {
				body = `{"model":"auto","stream":true,"messages":[{"role":"user","content":"go"}],"max_tokens":20}`
			} else {
				body = `{"model":"auto","messages":[{"role":"user","content":"go"}],"max_tokens":20}`
			}
			start := time.Now()
			req, _ := http.NewRequest(http.MethodPost, base+"/v1/coding/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer dummy")
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				errCount.Add(1)
				done.Add(1)
				continue
			}
			io.Copy(io.Discard, resp.Body) // drain (incl. full stream)
			resp.Body.Close()
			lat := time.Since(start)
			statusMu.Lock()
			statusMap[resp.StatusCode]++
			statusMu.Unlock()
			latMu.Lock()
			latencies = append(latencies, lat)
			latMu.Unlock()
			done.Add(1)
		}
	}

	// Concurrent /health + /metrics pollers — stress the read-lock path against
	// the breaker write-lock churn from all the injected failures.
	stopHealth := make(chan struct{})
	var healthWG sync.WaitGroup
	for i := 0; i < healthPollers; i++ {
		healthWG.Add(1)
		go func() {
			defer healthWG.Done()
			for {
				select {
				case <-stopHealth:
					return
				default:
					r, err := client.Get(base + "/health")
					if err != nil {
						healthBad.Add(1)
						continue
					}
					io.Copy(io.Discard, r.Body)
					r.Body.Close()
					if r.StatusCode == 200 {
						healthOK.Add(1)
					} else {
						healthBad.Add(1)
					}
				}
			}
		}()
	}

	// Progress ticker.
	stopTick := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopTick:
				return
			case <-t.C:
				fmt.Fprintf(os.Stderr, "  ...%d/%d done\n", done.Load(), total)
			}
		}
	}()

	wallStart := time.Now()
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go worker()
	}
	for i := 0; i < total; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	wall := time.Since(wallStart)
	close(stopHealth)
	close(stopTick)
	healthWG.Wait()

	// Report.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(p / 100 * float64(len(latencies)))
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}
	fmt.Println("========== STRESS RESULT ==========")
	fmt.Printf("requests:        %d over %d workers\n", total, conc)
	fmt.Printf("wall time:       %.2fs\n", wall.Seconds())
	fmt.Printf("throughput:      %.0f req/s\n", float64(done.Load())/wall.Seconds())
	fmt.Printf("transport errs:  %d\n", errCount.Load())
	fmt.Printf("latency p50/p95/p99/max: %v / %v / %v / %v\n", pct(50), pct(95), pct(99), pct(100))
	fmt.Println("status distribution:")
	statusMu.Lock()
	codes := make([]int, 0, len(statusMap))
	for c := range statusMap {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	for _, c := range codes {
		fmt.Printf("  %d: %d\n", c, statusMap[c])
	}
	statusMu.Unlock()
	fmt.Printf("health polls:    ok=%d bad=%d\n", healthOK.Load(), healthBad.Load())
	fmt.Println("===================================")
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
