package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Backend struct {
	URL          *url.URL
	Proxy        *httputil.ReverseProxy
	mu           sync.RWMutex
	healthy      bool
	connections  int32
	lastCheck    time.Time
}

func (b *Backend) setHealthy(h bool) {
	b.mu.Lock()
	b.healthy = h
	b.lastCheck = time.Now()
	b.mu.Unlock()
}

func (b *Backend) isHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthy
}

func (b *Backend) incConn() { atomic.AddInt32(&b.connections, 1) }
func (b *Backend) decConn() { atomic.AddInt32(&b.connections, -1) }
func (b *Backend) conns() int { return int(atomic.LoadInt32(&b.connections)) }

type LoadBalancer struct {
	backends []*Backend
	mu       sync.RWMutex
	rr       uint32
}

func NewLoadBalancer() *LoadBalancer {
	return &LoadBalancer{backends: make([]*Backend, 0)}
}

func (lb *LoadBalancer) AddBackend(addr string) error {
	u, err := url.Parse("http://" + addr)
	if err != nil {
		return err
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", "http")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Backend %s error: %v", addr, err)
		http.Error(w, "Backend unavailable", http.StatusBadGateway)
	}
	b := &Backend{URL: u, Proxy: proxy, healthy: true}
	lb.mu.Lock()
	lb.backends = append(lb.backends, b)
	lb.mu.Unlock()
	log.Printf("Added backend: %s", addr)
	return nil
}

func (lb *LoadBalancer) RemoveBackend(addr string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for i, b := range lb.backends {
		if b.URL.Host == addr {
			lb.backends = append(lb.backends[:i], lb.backends[i+1:]...)
			log.Printf("Removed backend: %s", addr)
			return
		}
	}
}

func (lb *LoadBalancer) NextBackend() *Backend {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	n := len(lb.backends)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		idx := atomic.AddUint32(&lb.rr, 1) % uint32(n)
		b := lb.backends[idx]
		if b.isHealthy() {
			return b
		}
	}
	return nil
}

func (lb *LoadBalancer) HealthCheck(ctx context.Context) {
	lb.mu.RLock()
	backends := make([]*Backend, len(lb.backends))
	copy(backends, lb.backends)
	lb.mu.RUnlock()

	for _, b := range backends {
		go func(b *Backend) {
			client := &http.Client{Timeout: 2 * time.Second}
			req, _ := http.NewRequestWithContext(ctx, "GET", b.URL.String()+"/ready", nil)
			resp, err := client.Do(req)
			healthy := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				resp.Body.Close()
			}
			b.setHealthy(healthy)
		}(b)
	}
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := lb.NextBackend()
	if b == nil {
		http.Error(w, "No healthy backends", http.StatusServiceUnavailable)
		return
	}
	b.incConn()
	defer b.decConn()
	b.Proxy.ServeHTTP(w, r)
}

func (lb *LoadBalancer) DrainBackend(addr string, timeout time.Duration) {
	lb.mu.RLock()
	var b *Backend
	for _, be := range lb.backends {
		if be.URL.Host == addr {
			b = be
			break
		}
	}
	lb.mu.RUnlock()
	if b == nil {
		return
	}
	b.setHealthy(false)
	log.Printf("Draining backend %s...", addr)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.conns() == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	lb.RemoveBackend(addr)
	log.Printf("Backend %s drained and removed", addr)
}

func main() {
	lbPort := getEnv("LB_PORT", "8080")
	backendStartPort := getEnvInt("BACKEND_START_PORT", 8081)
	maxBackends := getEnvInt("MAX_BACKENDS", 3)

	lb := NewLoadBalancer()

	// Start initial backend
	if err := lb.AddBackend(fmt.Sprintf("127.0.0.1:%d", backendStartPort)); err != nil {
		log.Fatalf("Failed to add initial backend: %v", err)
	}

	// Health check loop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lb.HealthCheck(ctx)
			}
		}
	}()

	// HTTP server
	srv := &http.Server{
		Addr:    ":" + lbPort,
		Handler: lb,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down load balancer...")
		cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("Load balancer listening on :%s", lbPort)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Load balancer stopped")
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := fmt.Sscanf(v, "%d", new(int)); err == nil {
			return i
		}
	}
	return def
}