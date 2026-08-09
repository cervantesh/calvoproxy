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
      coding:    {context_length: 128000}
      simple:    {context_length: 128000}
      reasoning: {context_length: 128000}
      vision:    {context_length: 128000}`, baseURL, baseURL)
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
			return unquoteYAML(stripYAMLComment(raw[len(prefix):]))
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
			return unquoteYAML(stripYAMLComment(trimmed[len(child)+1:]))
		}
	}
	return ""
}

// yamlHasTopLevelKey reports whether a top-level `key:` exists at all.
func yamlHasTopLevelKey(lines []string, key string) bool {
	return len(yamlBlock(lines, key)) > 0
}

// yamlBlock returns the indented body of a top-level `key:`, i.e. every line
// until the next top-level key. Scoping matters: `model:` also carries a
// base_url, so searching the whole file for one would let a correct
// model.base_url mask a custom_providers entry pointing somewhere else.
func yamlBlock(lines []string, key string) []string {
	start := -1
	for i, raw := range lines {
		if raw == strings.TrimLeft(raw, " \t") && strings.HasPrefix(raw, key+":") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []string
	for _, raw := range lines[start:] {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, raw)
			continue
		}
		if raw == strings.TrimLeft(raw, " \t") {
			break // next top-level key ends the block
		}
		out = append(out, raw)
	}
	return out
}

// yamlAnyScalar returns the value of `key:` at any indentation within lines.
// Used for keys that live inside a list item (custom_providers entries).
func yamlAnyScalar(lines []string, key string) string {
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		trimmed = strings.TrimPrefix(trimmed, "- ") // list item on the same line
		trimmed = strings.TrimSpace(trimmed)
		if strings.HasPrefix(trimmed, key+":") {
			return unquoteYAML(stripYAMLComment(trimmed[len(key)+1:]))
		}
	}
	return ""
}

// stripYAMLComment drops a trailing ` # ...` comment. Only when the '#' is
// preceded by whitespace, so a value like http://h/#frag survives — per the
// YAML spec, a '#' not preceded by whitespace is part of the scalar.
func stripYAMLComment(s string) string {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return s[:i]
		}
	}
	return s
}

// unquoteYAML removes matching surrounding quotes. `provider: "custom"` is
// perfectly valid YAML, and reading it line-wise would otherwise compare
// `"custom"` against `custom` and report a bogus FAIL.
func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// normBaseURL mirrors Hermes' own comparison (`strip().rstrip("/").lower()`),
// so doctor agrees with Hermes about whether two base_urls are the same route.
func normBaseURL(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
}

// yamlListEntries splits a block of list items into one []string per `- ` item,
// so a key can be read from the specific entry it belongs to rather than from
// whichever entry happens to come first.
func yamlListEntries(block []string) [][]string {
	var entries [][]string
	var cur []string
	for _, raw := range block {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			if len(cur) > 0 {
				entries = append(entries, cur)
			}
			cur = []string{raw}
			continue
		}
		if cur != nil {
			cur = append(cur, raw)
		}
	}
	if len(cur) > 0 {
		entries = append(entries, cur)
	}
	return entries
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

// checkReachable probes /health. The third return says whether the proxy
// answered at all (as opposed to the connection failing): /v1/chat/completions
// is not admin-gated, so a 401 on /health must not stop the live check.
func checkReachable(client *http.Client, base string) (checkResult, map[string]any, bool) {
	req, err := http.NewRequest(http.MethodGet, base+"/health", nil)
	if err != nil {
		return checkResult{status: statusFail, title: "Proxy reachable at " + base, detail: err.Error()}, nil, false
	}
	// /health is behind PROXY_ADMIN_TOKEN when that is set. Present it when we
	// have it, so a hardened install is not reported red for being hardened.
	if tok := strings.TrimSpace(os.Getenv("PROXY_ADMIN_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return checkResult{
			status: statusFail,
			title:  "Proxy reachable at " + base,
			detail: err.Error(),
			fix: "Start it:  calvoproxy\n" +
				"Or with Docker:  docker run -p 8080:8080 -e OPENROUTER_API_KEY=... ghcr.io/cervantesh/calvoproxy",
		}, nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return checkResult{
			status: statusWarn,
			title:  "Proxy reachable at " + base,
			detail: "/health is gated by PROXY_ADMIN_TOKEN and no matching token was presented,\n" +
				"so credential and model-policy details cannot be inspected.",
			fix: "Set PROXY_ADMIN_TOKEN in doctor's environment to the proxy's value.\n" +
				"The live completion check below does not need it.",
		}, nil, true
	}
	if resp.StatusCode != http.StatusOK {
		return checkResult{
			status: statusFail,
			title:  "Proxy reachable at " + base,
			detail: fmt.Sprintf("/health returned HTTP %d", resp.StatusCode),
		}, nil, true
	}
	var health map[string]any
	if err := json.Unmarshal(body, &health); err != nil {
		return checkResult{
			status: statusWarn,
			title:  "Proxy reachable at " + base,
			detail: "/health did not return JSON — is something else on this port?",
		}, nil, true
	}
	return checkResult{status: statusOK, title: "Proxy reachable at " + base}, health, true
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

	// Scoped to the custom_providers block on purpose: model.base_url carries
	// the same URL, so a file-wide search would report OK even when the
	// custom_providers entry points somewhere else entirely.
	providers := yamlBlock(lines, "custom_providers")
	if len(providers) == 0 {
		out = append(out, checkResult{
			status: statusFail,
			title:  "custom_providers entry exists",
			detail: "`provider: custom` alone is not enough — Hermes matches the provider by base_url.",
			fix:    "Add the custom_providers block printed below.",
		})
	} else {
		// Find the entry Hermes itself would select: the one whose base_url
		// matches model.base_url under Hermes' own normalization
		// (strip + rstrip("/") + lower). Selecting the entry first — rather
		// than scanning for any line that mentions the proxy URL — is what
		// makes the remaining per-entry checks apply to the right provider.
		var selected []string
		for _, entry := range yamlListEntries(providers) {
			if normBaseURL(yamlAnyScalar(entry, "base_url")) == normBaseURL(modelURL) && modelURL != "" {
				selected = entry
				break
			}
		}

		switch {
		case selected == nil:
			// The pair does not agree, so Hermes binds nothing and quietly
			// falls back to OpenRouter. This must be FAIL: it is the exact
			// silent bypass doctor exists to catch, and checking each side
			// against the proxy URL independently would miss it.
			out = append(out, checkResult{
				status: statusFail,
				title:  "custom_providers entry matches model.base_url",
				detail: fmt.Sprintf("no custom_providers entry has base_url %q\n"+
					"Hermes binds `provider: custom` by matching these two values;\n"+
					"when they disagree it silently uses OpenRouter instead.", modelURL),
				fix: "Make the entry's base_url identical to model.base_url (" + wantURL + ").",
			})
		case normBaseURL(yamlAnyScalar(selected, "base_url")) != normBaseURL(wantURL):
			out = append(out, checkResult{
				status: statusWarn,
				title:  "custom_providers entry matches model.base_url",
				detail: "the pair agrees, but points at " + yamlAnyScalar(selected, "base_url") +
					" while this proxy answers on " + wantURL,
			})
		default:
			out = append(out, checkResult{status: statusOK, title: "custom_providers entry matches model.base_url"})
		}

		if selected != nil {
			// Read from the SELECTED entry: with several providers configured,
			// the first `discover_models:` in the block may belong to another one.
			if strings.EqualFold(yamlAnyScalar(selected, "discover_models"), "true") {
				out = append(out, checkResult{
					status: statusWarn,
					title:  "discover_models disabled",
					detail: "the proxy serves no /v1/models; discovery will 404",
					fix:    "Set discover_models: false on the calvoproxy entry.",
				})
			}
			if mode := yamlAnyScalar(selected, "api_mode"); mode != "" && mode != "chat_completions" {
				out = append(out, checkResult{
					status: statusWarn,
					title:  "api_mode is chat_completions",
					detail: fmt.Sprintf("entry declares api_mode: %q; the proxy speaks the OpenAI chat API", mode),
					fix:    "Set api_mode: chat_completions.",
				})
			}
		}
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
	reach, health, answered := checkReachable(client, base)
	results = append(results, reach)

	if health != nil {
		results = append(results, checkAPIKey(health))
	}
	// Run the live check whenever the proxy answered at all — it is not
	// admin-gated, so it still works when /health is behind a token, and it is
	// the check that actually proves the chain end to end.
	if answered && !skipLive {
		profile := "simple"
		if mp, ok := health["model_policy"].(map[string]any); ok {
			if dp, ok := mp["default_profile"].(string); ok && dp != "" {
				profile = dp
			}
		}
		results = append(results, checkRoundTrip(client, base, profile))
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
