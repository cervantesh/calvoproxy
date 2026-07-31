package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultOpenRouterAuthURL = "https://openrouter.ai/auth"
	defaultOpenRouterKeysURL = "https://openrouter.ai/api/v1/auth/keys"
	loginTimeout             = 3 * time.Minute
)

// loopbackOverrideOr honours an env override for the OAuth endpoints ONLY when it
// points at loopback. Tests use an httptest server on 127.0.0.1; a shipped binary
// can't be redirected to an attacker's host (which would phish the login and
// steal the code+verifier).
func loopbackOverrideOr(env, def string) string {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	u, err := url.Parse(v)
	if err != nil {
		return def
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost") {
		return v
	}
	return def
}

func openrouterAuthURL() string {
	return loopbackOverrideOr("PROXY_OPENROUTER_AUTH_URL", defaultOpenRouterAuthURL)
}
func openrouterKeysURL() string {
	return loopbackOverrideOr("PROXY_OPENROUTER_KEYS_URL", defaultOpenRouterKeysURL)
}

// --- PKCE (RFC 7636, S256) ---

func randomURLToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- token exchange (hardened) ---

// oauthHTTPClient refuses redirects (a redirect on the keys endpoint could POST
// the code+verifier off-box) and bounds time.
var oauthHTTPClient = &http.Client{
	Timeout: 25 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("unexpected redirect during token exchange")
	},
}

func exchangeCodeForKey(ctx context.Context, code, verifier string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": "S256",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openrouterKeysURL(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		if resp != nil { // a blocked redirect returns both a response and an error
			resp.Body.Close()
		}
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	key := strings.TrimSpace(out.Key)
	if !looksLikeOpenRouterKey(key) {
		return "", errors.New("token exchange returned an unexpected key format")
	}
	return key, nil
}

// --- loopback callback server ---

type callbackResult struct {
	code string
	err  error
}

// validAuthCode is a conservative check on the returned code (non-empty, bounded,
// URL-token charset) so garbage never reaches the exchange.
func validAuthCode(code string) bool {
	if code == "" || len(code) > 512 {
		return false
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '~') {
			return false
		}
	}
	return true
}

const closeTabHTML = `<!doctype html><meta charset="utf-8"><title>CalvoProxy</title>` +
	`<body style="font-family:sans-serif;text-align:center;margin-top:4rem">` +
	`<h2>%s</h2><p>You can close this tab and return to the terminal.</p></body>`

func writeCloseTab(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// msg is one of our own fixed strings — never request-derived — so this is
	// not an injection sink.
	fmt.Fprintf(w, closeTabHTML, msg)
}

// startCallbackServer binds a single-use loopback listener and returns the chosen
// port and a channel that fires once with the first valid callback (or an error).
func startCallbackServer(ctx context.Context, expectedState string) (int, <-chan callbackResult, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, nil, err
	}
	ch := make(chan callbackResult, 1)
	var once sync.Once
	deliver := func(r callbackResult) { once.Do(func() { ch <- r }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		// Check state first, and NEVER consume the single-shot delivery for a
		// request that fails it: otherwise any local process could poison the
		// login by hitting /callback with junk before the real redirect arrives.
		stateOK := subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(expectedState)) == 1
		if st := q.Get("state"); st != "" && !stateOK {
			writeCloseTab(w, "State check failed.")
			return // ignored, not delivered
		}
		if e := q.Get("error"); e != "" {
			// A denial is only a definitive answer when we can attribute it to OUR
			// flow. An unattributed error= is pure denial-of-service (it would burn
			// the single delivery), so ignore it and let the real redirect — or the
			// timeout — decide.
			if !stateOK {
				writeCloseTab(w, "Ignoring an unattributed callback.")
				return
			}
			writeCloseTab(w, "Authorization was denied.")
			deliver(callbackResult{err: errors.New("authorization denied by user or provider")})
			return
		}
		code := q.Get("code")
		if !validAuthCode(code) {
			writeCloseTab(w, "Missing authorization code.")
			return // ignored: keep waiting for a well-formed callback (or timeout)
		}
		// A code with no state is still accepted (the provider may not echo state;
		// PKCE plus the single-use loopback listener protect the exchange itself),
		// but it is logged so an unexpected flow is visible.
		if !stateOK {
			fmt.Println("Note: the provider did not echo the CSRF state parameter; relying on PKCE.")
		}
		writeCloseTab(w, "Login complete ✓")
		deliver(callbackResult{code: code})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop := func() { _ = srv.Close() }
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ln.Addr().(*net.TCPAddr).Port, ch, stop, nil
}

// openBrowser best-effort opens a URL. Returns false on failure (caller prints the
// URL). Uses forms that don't re-parse the URL's `&` through a shell.
func openBrowser(rawURL string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		cmd = exec.Command("open", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start() == nil
}

// --- subcommands ---

func runLogin(args []string) int {
	noBrowser := false
	for _, a := range args {
		switch a {
		case "--no-browser":
			noBrowser = true
		case "--key-stdin":
			return loginKeyStdin()
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	verifier, err := randomURLToken(32)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate PKCE verifier:", err)
		return 1
	}
	state, err := randomURLToken(32)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate state:", err)
		return 1
	}

	port, ch, stop, err := startCallbackServer(ctx, state)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start callback server:", err)
		return 1
	}
	defer stop()

	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	authURL := openrouterAuthURL() + "?" + url.Values{
		"callback_url":          {callbackURL},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode()

	fmt.Println("Sign in to OpenRouter to authorize CalvoProxy…")
	if noBrowser || !openBrowser(authURL) {
		fmt.Println("Open this URL in your browser:")
		fmt.Println("  " + authURL)
	}

	select {
	case res := <-ch:
		if res.err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", res.err)
			return 1
		}
		key, err := exchangeCodeForKey(ctx, res.code, verifier)
		if err != nil {
			fmt.Fprintln(os.Stderr, "token exchange failed:", err)
			return 1
		}
		if err := storeAPIKey(key); err != nil {
			fmt.Fprintln(os.Stderr, "store key:", err)
			return 1
		}
		fmt.Printf("Logged in. Stored key %s at %s\n", maskKey(key), keyFilePath())
		return 0
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "login timed out — no authorization received.")
		return 1
	}
}

// loginKeyStdin stores a key piped on stdin — a browserless escape hatch for
// headless/CI use (`echo sk-or-... | calvoproxy login --key-stdin`).
func loginKeyStdin() int {
	data, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	key := strings.TrimSpace(string(data))
	if !looksLikeOpenRouterKey(key) {
		fmt.Fprintln(os.Stderr, "expected an OpenRouter key (sk-or-…) on stdin")
		return 1
	}
	if err := storeAPIKey(key); err != nil {
		fmt.Fprintln(os.Stderr, "store key:", err)
		return 1
	}
	fmt.Printf("Stored key %s at %s\n", maskKey(key), keyFilePath())
	return 0
}

func runLogout() int {
	if err := deleteAPIKey(); err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		return 1
	}
	fmt.Println("Logged out (stored key removed).")
	return 0
}

func runWhoami() int {
	if envKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); envKey != "" {
		fmt.Printf("Key configured via OPENROUTER_API_KEY (env): %s\n", maskKey(envKey))
		return 0
	}
	if k := storedAPIKey(); k != "" {
		fmt.Printf("Key configured via login file (%s): %s\n", keyFilePath(), maskKey(k))
		return 0
	}
	fmt.Println("No API key configured. Run `calvoproxy login`, or set OPENROUTER_API_KEY.")
	return 1
}
