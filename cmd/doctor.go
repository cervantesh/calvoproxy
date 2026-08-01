package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `calvoproxy doctor` — first-run self-check.
//
// Every check here exists because it cost real debugging time. The ordering is
// the order a newcomer actually hits the failures: is the proxy up, does it
// have a key, does a real request survive the whole chain, and finally is the
// Hermes side wired to point at us. Each failure prints the fix, not just the
// symptom.
//
// Deliberately dependency-free: the repo builds fully vendored/offline, so the
// Hermes YAML is inspected line-wise rather than parsed. That is a heuristic —
// it reads top-level keys and the one nested key we care about, and says so
// when it cannot be sure.

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) label() string {
	switch s {
	case statusOK:
		return "OK  "
	case statusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

type checkResult struct {
	status checkStatus
	title  string
	detail string
	fix    string
}

func (r checkResult) print() {
	fmt.Printf("[%s] %s\n", r.status.label(), r.title)
	if r.detail != "" {
		for _, line := range strings.Split(r.detail, "\n") {
			fmt.Printf("       %s\n", line)
		}
	}
	if r.fix != "" {
		for _, line := range strings.Split(r.fix, "\n") {
			fmt.Printf("       → %s\n", line)
		}
	}
}

// hermesConfigBlock is the exact YAML a user must have for Hermes to route
// through the proxy. Both halves are required: `model.base_url` is what binds
// `provider: custom` to the custom_providers entry, and Hermes only trusts that
// base_url when its host is loopback.
func hermesConfigBlock(baseURL string) string {
	return fmt.Sprintf(`model:
  provider: custom
  default: coding
  base_url: %s     # must be loopback (127.0.0.1), not a hostname
  api_key: dummy         # the real OpenRouter key lives in the proxy

custom_providers:
  - name: calvoproxy
    base_url: %s
    api_key: dummy
    api_mode: chat_completions
    discover_models: false   # the proxy does not expose /v1/models
    models:
      coding:    {context_length: 131072}
      simple:    {context_length: 131072}
      reasoning: {context_length: 131072}
      vision:    {context_length: 131072}`, baseURL, baseURL)
}

// Loopback detection reuses isLoopbackHost from security.go: Hermes refuses to
// honor model.base_url for a bare `custom` provider unless the host is
// loopback, so a non-loopback value silently routes to OpenRouter instead.

// yamlScalar returns the value of a top-level `key:` scalar, or "" if absent.
// Line-wise on purpose (see file header): good enough for the flat keys we
// check, and it never rewrites the user's file.
func yamlScalar(lines []string, key string) string {
	prefix := key + ":"
	for _, raw := range lines {
		if strings.HasPrefix(raw, "#") || strings.TrimSpace(raw) == "" {
			continue
		}
		if raw != strings.TrimLeft(raw, " \t") {
			continue // indented: not top-level
		}
		if strings.HasPrefix(raw, prefix) {
			return strings.TrimSpace(stripYAMLComment(raw[len(prefix):]))
		}
	}
	return ""
}

// yamlNestedScalar returns `parent:` → `  child:` for a one-level nesting.
func yamlNestedScalar(lines []string, parent, child string) string {
	inParent := false
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		isTopLevel := raw == strings.TrimLeft(raw, " \t")
		if isTopLevel {
			inParent = strings.HasPrefix(raw, parent+":")
			continue
		}
		if inParent && strings.HasPrefix(trimmed, child+":") {
			return strings.TrimSpace(stripYAMLComment(trimmed[len(child)+1:]))
		}
	}
	return ""
}

// yamlHasTopLevelKey reports whether a top-level `key:` exists at all.
func yamlHasTopLevelKey(lines []string, key string) bool {
	for _, raw := range lines {
		if raw == strings.TrimLeft(raw, " \t") && strings.HasPrefix(raw, key+":") {
			return true
		}
	}
	return false
}

// stripYAMLComment drops a trailing ` # ...` comment. Only when the '#' is
// preceded by whitespace, so a value like http://h/#frag survives.
func stripYAMLComment(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

// hermesConfigPath locates Hermes' config.yaml the same way Hermes does:
// $HERMES_HOME wins, then the per-OS default. Returns "" when nothing exists.
func hermesConfigPath() string {
	if h := strings.TrimSpace(os.Getenv("HERMES_HOME")); h != "" {
		p := filepath.Join(h, "config.yaml")
		if fileExists(p) {
			return p
		}
	}
	var candidates []string
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		candidates = append(candidates, filepath.Join(local, "hermes", "config.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".hermes", "config.yaml"))
	}
	for _, p := range candidates {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// proxyBaseURL is where this machine should reach the proxy, honoring the same
// env the server honors so `doctor` checks the instance the user actually runs.
func proxyBaseURL() string {
	host := envOrDefault("HOST", "127.0.0.1")
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1" // bound to everything; reach it over loopback
	}
	return "http://" + net.JoinHostPort(host, envOrDefault("PORT", "8080"))
}

func checkReachable(client *http.Client, base string) (checkResult, map[string]any) {
	resp, err := client.Get(base + "/health")
	if err != nil {
		return checkResult{
			status: statusFail,
			title:  "Proxy reachable at " + base,
			detail: err.Error(),
			fix: "Start it:  calvoproxy\n" +
				"Or with Docker:  docker run -p 8080:8080 -e OPENROUTER_API_KEY=... ghcr.io/cervantesh/calvoproxy",
		}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return checkResult{
			status: statusFail,
			title:  "Proxy reachable at " + base,
			detail: fmt.Sprintf("/health returned HTTP %d", resp.StatusCode),
		}, nil
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		return checkResult{
			status: statusWarn,
			title:  "Proxy reachable at " + base,
			detail: "/health did not return JSON — is something else on this port?",
		}, nil
	}
	return checkResult{status: statusOK, title: "Proxy reachable at " + base}, health
}

func checkAPIKey(health map[string]any) checkResult {
	if configured, _ := health["configured_api_key"].(bool); configured {
		src := "OPENROUTER_API_KEY (environment)"
		if strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) == "" {
			if p := keyFilePath(); p != "" && fileExists(p) {
				src = "login key file: " + p
			} else {
				src = "configured"
			}
		}
		return checkResult{status: statusOK, title: "OpenRouter credentials present", detail: src}
	}
	return checkResult{
		status: statusFail,
		title:  "OpenRouter credentials present",
		fix: "calvoproxy login                       (browser sign-in, per-user key)\n" +
			"or set OPENROUTER_API_KEY in the proxy's environment",
	}
}

// checkRoundTrip is the check that matters most: it proves the whole chain —
// credentials, upstream reachability and at least one live free model — with
// the same request shape Hermes sends. A green light here means any remaining
// failure is on the client side.
func checkRoundTrip(client *http.Client, base string, profile string) checkResult {
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"max_tokens":8}`, profile)
	resp, err := client.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		return checkResult{status: statusFail, title: "Live completion through the chain", detail: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return checkResult{
			status: statusFail,
			title:  "Live completion through the chain",
			detail: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 300)),
			fix: "429/503 usually means every free model in the profile is rate-limited — retry shortly.\n" +
				"401/403 means the OpenRouter key is rejected — re-run `calvoproxy login`.",
		}
	}
	var parsed struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(body, &parsed)
	detail := "profile " + profile
	if parsed.Model != "" {
		detail += " → " + parsed.Model
	}
	return checkResult{status: statusOK, title: "Live completion through the chain", detail: detail}
}

// checkHermes validates the client-side wiring. Both failures below were real:
// a missing model.base_url silently routes to openrouter.ai, and a
// custom_providers entry whose base_url does not match is never selected.
func checkHermes(base string) []checkResult {
	path := hermesConfigPath()
	if path == "" {
		return []checkResult{{
			status: statusWarn,
			title:  "Hermes integration",
			detail: "No Hermes config.yaml found — skipping (fine if you only use the HTTP API).",
			fix:    "Set HERMES_HOME if your install lives elsewhere.",
		}}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return []checkResult{{status: statusFail, title: "Hermes integration", detail: "cannot read " + path + ": " + err.Error()}}
	}
	// Strip a UTF-8 BOM: Windows tooling (PowerShell's Set-Content, and Hermes
	// itself) writes config.yaml with one, which would otherwise hide the very
	// first top-level key — `model:` — and produce a bogus FAIL.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}

	out := []checkResult{{status: statusOK, title: "Hermes config found", detail: path}}
	wantURL := base + "/v1"

	provider := yamlNestedScalar(lines, "model", "provider")
	if provider != "custom" {
		out = append(out, checkResult{
			status: statusFail,
			title:  "model.provider is custom",
			detail: fmt.Sprintf("found %q", provider),
			fix:    "Set it to `custom`, then apply the block printed below.",
		})
	} else {
		out = append(out, checkResult{status: statusOK, title: "model.provider is custom"})
	}

	modelURL := yamlNestedScalar(lines, "model", "base_url")
	switch {
	case modelURL == "":
		out = append(out, checkResult{
			status: statusFail,
			title:  "model.base_url points at the proxy",
			detail: "model.base_url is missing — this is the single most common cause of\n" +
				"requests silently going to openrouter.ai instead of the proxy.",
			fix: "Add  base_url: " + wantURL + "  under `model:`.",
		})
	default:
		host := ""
		if u, err := url.Parse(modelURL); err == nil {
			host = u.Hostname()
		}
		switch {
		case !isLoopbackHost(host):
			out = append(out, checkResult{
				status: statusFail,
				title:  "model.base_url points at the proxy",
				detail: fmt.Sprintf("%q is not loopback; Hermes ignores it and falls back to OpenRouter", modelURL),
				fix:    "Use " + wantURL,
			})
		case strings.EqualFold(host, "localhost"):
			out = append(out, checkResult{
				status: statusWarn,
				title:  "model.base_url points at the proxy",
				detail: modelURL + " uses the hostname `localhost`, which can resolve to IPv6 ::1\n" +
					"while the proxy listens on IPv4 only.",
				fix: "Prefer the literal address: " + wantURL,
			})
		case strings.TrimRight(modelURL, "/") != strings.TrimRight(wantURL, "/"):
			out = append(out, checkResult{
				status: statusWarn,
				title:  "model.base_url points at the proxy",
				detail: fmt.Sprintf("config says %s but this proxy answers on %s", modelURL, wantURL),
			})
		default:
			out = append(out, checkResult{status: statusOK, title: "model.base_url points at the proxy", detail: modelURL})
		}
	}

	if !yamlHasTopLevelKey(lines, "custom_providers") {
		out = append(out, checkResult{
			status: statusFail,
			title:  "custom_providers entry exists",
			detail: "`provider: custom` alone is not enough — Hermes matches the provider by base_url.",
			fix:    "Add the custom_providers block printed below.",
		})
	} else {
		hasURL := false
		for _, l := range lines {
			if strings.Contains(l, strings.TrimRight(wantURL, "/")) && strings.Contains(l, "base_url") {
				hasURL = true
				break
			}
		}
		if !hasURL {
			out = append(out, checkResult{
				status: statusWarn,
				title:  "custom_providers entry exists",
				detail: "found custom_providers, but no entry whose base_url is " + wantURL,
				fix:    "Hermes selects the provider by matching base_url — they must be identical.",
			})
		} else {
			out = append(out, checkResult{status: statusOK, title: "custom_providers entry exists"})
		}
	}

	if v := yamlScalar(lines, "discover_models"); v == "true" {
		out = append(out, checkResult{
			status: statusWarn,
			title:  "discover_models disabled",
			detail: "the proxy does not serve /v1/models; discovery will 404",
			fix:    "Set discover_models: false on the calvoproxy entry.",
		})
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func runDoctor(args []string) int {
	skipLive := false
	for _, a := range args {
		switch a {
		case "--no-live":
			skipLive = true
		case "-h", "--help":
			fmt.Println("Usage: calvoproxy doctor [--no-live]\n\n" +
				"Checks that the proxy is running, has credentials, can complete a real\n" +
				"request, and that Hermes is wired to route through it.\n\n" +
				"  --no-live   skip the live completion (no upstream tokens spent)")
			return 0
		}
	}

	fmt.Println("CalvoProxy doctor " + version)
	fmt.Println()

	client := &http.Client{Timeout: 60 * time.Second}
	base := proxyBaseURL()

	var results []checkResult
	reach, health := checkReachable(client, base)
	results = append(results, reach)

	if health != nil {
		results = append(results, checkAPIKey(health))
		if !skipLive {
			profile := "simple"
			if mp, ok := health["model_policy"].(map[string]any); ok {
				if dp, ok := mp["default_profile"].(string); ok && dp != "" {
					profile = dp
				}
			}
			results = append(results, checkRoundTrip(client, base, profile))
		}
	}
	results = append(results, checkHermes(base)...)

	failed, warned := 0, 0
	for _, r := range results {
		r.print()
		switch r.status {
		case statusFail:
			failed++
		case statusWarn:
			warned++
		}
	}

	fmt.Println()
	if failed > 0 {
		fmt.Println("Hermes config block (paste into config.yaml, then restart the gateway):")
		fmt.Println()
		fmt.Println(hermesConfigBlock(base + "/v1"))
		fmt.Println()
		fmt.Println("The Hermes gateway does NOT reload config.yaml while running —")
		fmt.Println("restart it after editing, or the change has no effect.")
		fmt.Println()
		fmt.Printf("%d check(s) failed, %d warning(s).\n", failed, warned)
		return 1
	}
	if warned > 0 {
		fmt.Printf("All required checks passed, %d warning(s).\n", warned)
		return 0
	}
	fmt.Println("All checks passed — Hermes will route through CalvoProxy.")
	return 0
}
