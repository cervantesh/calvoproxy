package router

import "testing"

func TestSideEffectsDisabledByDefault(t *testing.T) {
	if sideEffectsFromEnv() != nil {
		t.Fatal("side effects must be nil (disabled) by default — no git/sqlite on the hot path")
	}
	t.Setenv("PROXY_WORKSPACE_SIDE_EFFECTS", "true")
	if sideEffectsFromEnv() == nil {
		t.Fatal("side effects should be enabled when PROXY_WORKSPACE_SIDE_EFFECTS=true")
	}
}
