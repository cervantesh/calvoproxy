package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalModelPolicyFileOverridesEmbedded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-policy.json")
	if err := os.WriteFile(path, []byte(`{
		"DefaultProfile": "coding",
		"Profiles": {"coding": ["test/model-from-file:free"]},
		"Aliases": {"coding": "coding"}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODEL_POLICY_FILE", path)

	rt := loadModelPolicyRuntime()
	got := rt.Config.Profiles["coding"]
	if len(got) != 1 || got[0] != "test/model-from-file:free" {
		t.Fatalf("expected coding chain from external file, got %+v", got)
	}
	if rt.Config.DefaultProfile != "coding" {
		t.Fatalf("expected DefaultProfile from file, got %q", rt.Config.DefaultProfile)
	}
}

func TestMissingExternalFileFallsBackToEmbedded(t *testing.T) {
	t.Setenv("PROXY_MODEL_POLICY_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))
	rt := loadModelPolicyRuntime()
	if len(rt.Config.Profiles) == 0 {
		t.Fatal("expected embedded default profiles when file is absent")
	}
}

func TestEnvJSONStillWinsOverExternalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-policy.json")
	os.WriteFile(path, []byte(`{"Profiles":{"coding":["from-file:free"]}}`), 0o644)
	t.Setenv("PROXY_MODEL_POLICY_FILE", path)
	t.Setenv("PROXY_PROVIDER_PROFILES_JSON", `{"coding":["from-env:free"]}`)

	rt := loadModelPolicyRuntime()
	got := rt.Config.Profiles["coding"]
	if len(got) != 1 || got[0] != "from-env:free" {
		t.Fatalf("expected env to win over file, got %+v", got)
	}
}
