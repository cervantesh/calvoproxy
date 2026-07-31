package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReloadModelPolicySwapsChains(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-policy.json")
	os.WriteFile(path, []byte(`{"DefaultProfile":"simple","Profiles":{"simple":["a:free"]},"Aliases":{"simple":"simple"}}`), 0o644)
	t.Setenv("PROXY_MODEL_POLICY_FILE", path)

	s := NewRouterService()
	if _, ok := s.getPolicy().Profiles["simple"]; !ok {
		t.Fatal("expected simple profile at startup")
	}
	if _, ok := s.getPolicy().Profiles["newprof"]; ok {
		t.Fatal("newprof should not exist yet")
	}
	os.WriteFile(path, []byte(`{"DefaultProfile":"simple","Profiles":{"simple":["a:free"],"newprof":["b:free"]},"Aliases":{"simple":"simple"}}`), 0o644)
	if err := s.ReloadModelPolicy(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if _, ok := s.getPolicy().Profiles["newprof"]; !ok {
		t.Fatal("expected newprof after hot reload")
	}
}
