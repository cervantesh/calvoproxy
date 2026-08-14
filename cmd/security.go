package main

import (
	"crypto/subtle"
	"net"
	"os"
	"strings"
)

// bindHost is the interface CalvoProxy bound to, captured at startup so the
// request path can reason about network exposure (e.g. whether to spend the
// env OpenRouter key for a keyless request). Set once in main() before serving.
var bindHost string

// adminPolicy is intentionally derived once at startup.  A public bind must
// never turn administrative endpoints into an unauthenticated control plane
// simply because an operator omitted PROXY_ADMIN_TOKEN.
type adminPolicy struct {
	public       bool
	token        string
	requireToken bool
}

var currentAdminPolicy adminPolicy

func configureAdminPolicy(host string) {
	bindHost = host
	token := strings.TrimSpace(os.Getenv("PROXY_ADMIN_TOKEN"))
	currentAdminPolicy = adminPolicy{
		public:       !isLoopbackHost(host),
		token:        token,
		requireToken: !isLoopbackHost(host) || envTrue("PROXY_ADMIN_REQUIRE_TOKEN"),
	}
}

func envTrue(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (p adminPolicy) disabled() bool { return p.requireToken && p.token == "" }

// activeAdminPolicy also makes handler-level construction safe in tests and
// embedders that use newMux without running main first. Production still
// configures the policy once at startup.
func activeAdminPolicy() adminPolicy {
	p := currentAdminPolicy
	if p.token == "" {
		p.token = strings.TrimSpace(os.Getenv("PROXY_ADMIN_TOKEN"))
	}
	if bindHost != "" {
		p.public = !isLoopbackHost(bindHost)
		p.requireToken = p.public || envTrue("PROXY_ADMIN_REQUIRE_TOKEN")
	}
	return p
}

// isLoopbackHost reports whether a bind host is loopback-only (safe to treat as
// private). Empty, 0.0.0.0 and :: mean "all interfaces" → public. A hostname is
// resolved; if every resolved IP is loopback it's private, otherwise public.
func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	addrs, err := net.LookupIP(h)
	if err != nil || len(addrs) == 0 {
		return false // unknown → treat as public (fail safe)
	}
	for _, ip := range addrs {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// boundToPublicInterface reports whether the proxy is reachable off-host.
func boundToPublicInterface() bool { return !isLoopbackHost(bindHost) }

// allowEnvKeyOnPublicBind reports whether the operator explicitly opted in to
// spending the env OPENROUTER_API_KEY for keyless requests while bound to a
// public interface (PROXY_ALLOW_ENV_KEY_PUBLIC). On a loopback bind the env key
// is always allowed (unchanged behaviour).
func allowEnvKeyOnPublicBind() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROXY_ALLOW_ENV_KEY_PUBLIC"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// constantTimeEqual compares two secrets without leaking their relationship
// through timing. Length mismatch short-circuits to false (acceptable — length
// is not the secret).
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
