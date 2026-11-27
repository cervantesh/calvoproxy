package main

import (
	"strings"
	"testing"
)

func childEnvValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return strings.TrimPrefix(kv, name+"="), true
		}
	}
	return "", false
}

// La clave de OpenRouter llega desde Bitwarden con otro nombre para que el
// borrado de la variable vieja no se la lleve por delante (2026-11-27).
func TestTheBitwardenOpenRouterKeyReachesTheProxy(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "clave-vieja-rechazada")
	t.Setenv("CALVOPROXY_OPENROUTER_API_KEY", "clave-de-bitwarden")
	env := buildChildEnv()
	if v, _ := childEnvValue(env, "OPENROUTER_API_KEY"); v != "clave-de-bitwarden" {
		t.Fatalf("OPENROUTER_API_KEY = %q, want the Bitwarden key", v)
	}
	if _, ok := childEnvValue(env, "CALVOPROXY_OPENROUTER_API_KEY"); ok {
		t.Fatal("the transport variable must not reach the proxy")
	}
}

func TestWithoutTheBitwardenKeyTheStaleOneIsStillDropped(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "clave-vieja-rechazada")
	t.Setenv("CALVOPROXY_OPENROUTER_API_KEY", "")
	if _, ok := childEnvValue(buildChildEnv(), "OPENROUTER_API_KEY"); ok {
		t.Fatal("the stale inherited key must still be dropped")
	}
}
