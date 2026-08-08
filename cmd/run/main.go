package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type BackendInfo struct {
	Port     int    `json:"port"`
	GRPCPort int    `json:"grpc_port"`
	PID      int    `json:"pid"`
	Started  string `json:"started"`
}

const (
	lbPort         = 8080
	backendStart   = 8081
	stateFile      = "calvoproxy-backends.json"
	maxBackends    = 3
	healthTimeout  = 10 * time.Second
	maxFailures    = 3           // consecutive health check failures before replacement
	healthInterval = 5 * time.Second
)

// daemonize detaches the process from the console on Windows
// and redirects stdout/stderr to log files
func daemonize(root, logDir string) error {
	if logDir == "" {
		logDir = root
	}
	
	outLog := filepath.Join(logDir, "lb-out.log")
	errLog := filepath.Join(logDir, "lb-err.log")

	// Open log files
	outFile, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open stdout log: %w", err)
	}
	errFile, err := os.OpenFile(errLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open stderr log: %w", err)
	}

	// Redirect stdout/stderr
	os.Stdout = outFile
	os.Stderr = errFile
	log.SetOutput(errFile)

	// Detach from console on Windows
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	freeConsole.Call()

	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "lb" {
		runLoadBalancer(os.Args[2:])
		return
	}
	runDeployer()
}

func runLoadBalancer(args []string) {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding repo root: %v\n", err)
		os.Exit(1)
	}

	// Parse flags for lb subcommand
	fs := flag.NewFlagSet("lb", flag.ExitOnError)
	daemon := fs.Bool("daemon", false, "Run as detached daemon (Windows)")
	logDir := fs.String("log-dir", "", "Log directory for daemon mode (default: repo root)")
	fs.Parse(args)

	// Daemonize on Windows if requested
	if *daemon && runtime.GOOS == "windows" {
		if err := daemonize(root, *logDir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to daemonize: %v\n", err)
			os.Exit(1)
		}
		// After daemonize, we're in the child process, continue normally
	}

	// Ensure calvoproxy.exe exists
	binPath := filepath.Join(root, "calvoproxy.exe")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		fmt.Println("Building calvoproxy.exe ...")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd")
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Build failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Start LB
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lb := NewLoadBalancer()

	// Start initial backend
	if err := startBackend(ctx, lb, root, backendStart); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start initial backend: %v\n", err)
		os.Exit(1)
	}

	// Health check loop
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
		Addr:    fmt.Sprintf(":%d", lbPort),
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

	log.Printf("Load balancer listening on :%d", lbPort)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	log.Println("Load balancer stopped")
}

func runDeployer() {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding repo root: %v\n", err)
		os.Exit(1)
	}

	// Check if LB is running
	if !isPortOpen("127.0.0.1", lbPort) {
		fmt.Println("Load balancer not running. Starting it in daemon mode...")
		cmd := exec.Command(os.Args[0], "lb", "-daemon")
		cmd.Dir = root
		cmd.Stdout = nil // Detached, logs go to files
		cmd.Stderr = nil
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start LB: %v\n", err)
			os.Exit(1)
		}
		// Don't wait - process detaches immediately
		time.Sleep(2 * time.Second)
	}

	// Find free backend port
	port := findFreePort(backendStart)
	grpcPort := findFreePort(19091)

	os.Setenv("PORT", strconv.Itoa(port))
	os.Setenv("GRPC_PORT", strconv.Itoa(grpcPort))
	os.Setenv("OTEL_ENABLED", "false")

	if runtime.GOOS == "windows" {
		if key := os.Getenv("OPENROUTER_API_KEY"); key == "" {
			if userKey := getUserEnv("OPENROUTER_API_KEY"); userKey != "" {
				os.Setenv("OPENROUTER_API_KEY", userKey)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: OPENROUTER_API_KEY not set\n")
			}
		}
	}

	binPath := filepath.Join(root, "calvoproxy.exe")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		fmt.Println("Building calvoproxy.exe ...")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd")
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Build failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Start backend
	cmd := exec.Command(binPath)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting backend: %v\n", err)
		os.Exit(1)
	}

	pid := cmd.Process.Pid
	fmt.Printf("Started backend on port %d (PID: %d)\n", port, pid)

	// Wait for health
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	if waitForHealth(ctx, port) {
		fmt.Printf("Backend healthy on port %d\n", port)
	} else {
		fmt.Fprintf(os.Stderr, "Backend health check failed\n")
		os.Exit(1)
	}

	// Save backend info
	saveBackend(BackendInfo{
		Port:     port,
		GRPCPort: grpcPort,
		PID:      pid,
		Started:  time.Now().Format(time.RFC3339),
	})

	// Wait for process
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "Backend exited: %v\n", err)
	}
	removeBackend(port)
}

func waitForHealth(ctx context.Context, port int) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/ready", port)
	// Transport with connection pooling for health checks
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 5 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          50,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return true
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

// Load Balancer implementation

type Backend struct {
	URL           *url.URL
	Proxy         *httputil.ReverseProxy
	mu            sync.RWMutex
	healthy       bool
	connections   int32
	lastCheck     time.Time
	failures      int       // consecutive health check failures
	port          int       // backend port for replacement
	replacing     bool      // prevents duplicate replacement attempts
}

func (b *Backend) setHealthy(h bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.healthy = h
	b.lastCheck = time.Now()
	if h {
		b.failures = 0
	} else {
		b.failures++
	}
}

func (b *Backend) isHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthy
}

func (b *Backend) failureCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.failures
}

func (b *Backend) shouldReplace() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return !b.healthy && b.failures >= maxFailures && !b.replacing
}

func (b *Backend) markReplacing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.replacing {
		return false
	}
	b.replacing = true
	return true
}

func (b *Backend) incConn() { atomic.AddInt32(&b.connections, 1) }
func (b *Backend) decConn() { atomic.AddInt32(&b.connections, -1) }
func (b *Backend) conns() int { return int(atomic.LoadInt32(&b.connections)) }

func (b *Backend) getPort() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.port
}

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
	// Extract port from address
	port := 0
	if host := u.Host; host != "" {
		if idx := strings.LastIndex(host, ":"); idx >= 0 {
			fmt.Sscanf(host[idx+1:], "%d", &port)
		}
	}
	// Optimized transport with connection pooling
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = transport
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
	b := &Backend{URL: u, Proxy: proxy, healthy: true, port: port}
	lb.mu.Lock()
	lb.backends = append(lb.backends, b)
	lb.mu.Unlock()
	log.Printf("Added backend: %s (port %d)", addr, port)
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

	// Shared transport for health checks with connection pooling
	healthTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 5 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          50,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
	}
	healthClient := &http.Client{
		Transport: healthTransport,
		Timeout:   3 * time.Second,
	}

	for _, b := range backends {
		go func(b *Backend) {
			req, _ := http.NewRequestWithContext(ctx, "GET", b.URL.String()+"/ready", nil)
			resp, err := healthClient.Do(req)
			healthy := err == nil && resp.StatusCode == http.StatusOK
			if resp != nil {
				resp.Body.Close()
			}
			b.setHealthy(healthy)

			// Check if backend needs replacement
			if b.shouldReplace() && b.markReplacing() {
				go lb.replaceBackend(context.Background(), b)
			}
		}(b)
	}
}

// replaceBackend stops the failed backend and starts a new one on a free port
func (lb *LoadBalancer) replaceBackend(ctx context.Context, failed *Backend) {
	addr := failed.URL.Host
	log.Printf("Backend %s failed %d times, replacing...", addr, maxFailures)

	// Drain and remove old backend
	lb.DrainBackend(addr, 10*time.Second)

	// Find free port for replacement
	newPort := findFreePort(backendStart)
	newAddr := fmt.Sprintf("127.0.0.1:%d", newPort)

	// Get repo root for startBackend
	root, err := findRepoRoot()
	if err != nil {
		log.Printf("Failed to find repo root for replacement: %v", err)
		return
	}

	// Start new backend
	if err := startBackend(ctx, lb, root, newPort); err != nil {
		log.Printf("Failed to start replacement backend on %s: %v", newAddr, err)
		return
	}

	log.Printf("Successfully replaced backend %s with %s", addr, newAddr)
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

func startBackend(ctx context.Context, lb *LoadBalancer, root string, port int) error {
	grpcPort := findFreePort(19091)
	os.Setenv("PORT", strconv.Itoa(port))
	os.Setenv("GRPC_PORT", strconv.Itoa(grpcPort))
	os.Setenv("OTEL_ENABLED", "false")

	binPath := filepath.Join(root, "calvoproxy.exe")
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}

	// Wait for health
	if waitForHealth(ctx, port) {
		return lb.AddBackend(fmt.Sprintf("127.0.0.1:%d", port))
	}
	cmd.Process.Kill()
	return fmt.Errorf("backend health check failed")
}

func findRepoRoot() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot determine caller")
	}
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found")
}

func findFreePort(start int) int {
	for port := start; port < 65535; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port
		}
	}
	return start
}

func isPortOpen(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func getUserEnv(key string) string {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", fmt.Sprintf(`reg query HKCU\Environment /v %s`, key)).Output()
		if err != nil {
			return ""
		}
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.Contains(line, key) {
				parts := strings.Fields(line)
				if len(parts) >= 3 {
					return parts[2]
				}
			}
		}
	}
	return ""
}

func saveBackend(info BackendInfo) {
	// Read existing backends
	var backends []BackendInfo
	if data, err := os.ReadFile(stateFile); err == nil {
		json.Unmarshal(data, &backends)
	}
	// Update or add this backend
	found := false
	for i, b := range backends {
		if b.Port == info.Port {
			backends[i] = info
			found = true
			break
		}
	}
	if !found {
		backends = append(backends, info)
	}
	data, _ := json.MarshalIndent(backends, "", "  ")
	os.WriteFile(stateFile, data, 0644)
}

func removeBackend(port int) {
	// Read existing backends and remove only the specified port
	if data, err := os.ReadFile(stateFile); err == nil {
		var backends []BackendInfo
		if json.Unmarshal(data, &backends) == nil {
			var filtered []BackendInfo
			for _, b := range backends {
				if b.Port != port {
					filtered = append(filtered, b)
				}
			}
			data, _ := json.MarshalIndent(filtered, "", "  ")
			os.WriteFile(stateFile, data, 0644)
		}
	}
}