package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupSandbox points every path lookup at a temp dir so a test can never touch
// the developer's real ~/.claude or ~/.codex. Writing into another program's
// config is the one destructive operation in this whole feature; the tests for
// it must not be able to perform it for real.
func setupSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(dir, "AppData", "Roaming"))
	t.Setenv("HERMES_HOME", "")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Invariant 1: --check is the default and never writes. Editing another
// program's config is the only destructive act in the plan, so the default has
// to be the one that cannot break anything.
func TestSetup_CheckNeverWrites(t *testing.T) {
	home := setupSandbox(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	original := "{\n  \"theme\": \"dark\"\n}\n"
	writeFile(t, settings, original)

	out := &strings.Builder{}
	runSetupWith([]string{"claude-code", "--url", "http://127.0.0.1:8080"}, out)

	if got := readFile(t, settings); got != original {
		t.Errorf("--check wrote to the config:\n%s", got)
	}
	if !strings.Contains(out.String(), "ANTHROPIC_BASE_URL") {
		t.Errorf("check should show the block it would write; got:\n%s", out)
	}
}

// Invariant 1b: with no config present, --check must not create one either.
func TestSetup_CheckDoesNotCreateMissingConfig(t *testing.T) {
	home := setupSandbox(t)
	out := &strings.Builder{}
	runSetupWith([]string{"claude-code"}, out)

	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("--check created the config file; it must only report")
	}
}

// Invariants 2 and 3: --apply backs up the original byte for byte and leaves
// unrelated keys alone. Clobbering a user's settings.json is the failure mode
// this whole design exists to prevent.
func TestSetup_ApplyBacksUpAndPreservesOtherKeys(t *testing.T) {
	home := setupSandbox(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	original := `{"theme":"dark","env":{"EDITOR":"vim"}}`
	writeFile(t, settings, original)

	out := &strings.Builder{}
	if code := runSetupWith([]string{"claude-code", "--apply", "--url", "http://127.0.0.1:8080"}, out); code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(readFile(t, settings)), &parsed); err != nil {
		t.Fatalf("apply produced invalid JSON: %v", err)
	}
	if parsed["theme"] != "dark" {
		t.Errorf("unrelated top-level key lost: %v", parsed)
	}
	env, _ := parsed["env"].(map[string]any)
	if env["EDITOR"] != "vim" {
		t.Errorf("unrelated env key lost: %v", env)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8080" {
		t.Errorf("base url not set: %v", env)
	}

	// os.UserConfigDir is APPDATA on Windows and XDG_CONFIG_HOME elsewhere; ask
	// it rather than hardcoding one platform's layout.
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(cfgDir, "calvoproxy", "backups", "claude-code-*.bak"))
	if len(backups) != 1 {
		t.Fatalf("expected exactly 1 backup, got %d", len(backups))
	}
	if got := readFile(t, backups[0]); got != original {
		t.Errorf("backup is not the original:\n%s", got)
	}
}

// Invariant 4: applying twice changes nothing the second time and says so.
func TestSetup_ApplyIsIdempotent(t *testing.T) {
	home := setupSandbox(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"theme":"dark"}`)

	first := &strings.Builder{}
	runSetupWith([]string{"claude-code", "--apply"}, first)
	afterFirst := readFile(t, settings)

	second := &strings.Builder{}
	runSetupWith([]string{"claude-code", "--apply"}, second)

	if got := readFile(t, settings); got != afterFirst {
		t.Errorf("second apply changed the file:\n%s", got)
	}
	if !strings.Contains(second.String(), "ya configurado") {
		t.Errorf("second apply should report it was already configured; got:\n%s", second)
	}
}

// Invariant 5: the TOML keeps its comments and prior content. There is no
// vendored TOML parser and a round-trip would eat the user's comments, so the
// block is marker-delimited and everything else is copied verbatim.
func TestSetup_CodexPreservesCommentsAndContent(t *testing.T) {
	home := setupSandbox(t)
	config := filepath.Join(home, ".codex", "config.toml")
	original := "# mi configuración\nmodel = \"gpt-5\"\n\n[history]\npersistence = \"save-all\"\n"
	writeFile(t, config, original)

	out := &strings.Builder{}
	if code := runSetupWith([]string{"codex", "--apply", "--url", "http://127.0.0.1:8080"}, out); code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out)
	}

	got := readFile(t, config)
	for _, want := range []string{"# mi configuración", `model = "gpt-5"`, "[history]", `persistence = "save-all"`} {
		if !strings.Contains(got, want) {
			t.Errorf("apply lost %q from the user's TOML:\n%s", want, got)
		}
	}
	if !strings.Contains(got, ">>> calvoproxy >>>") || !strings.Contains(got, "<<< calvoproxy <<<") {
		t.Errorf("block is not marker-delimited:\n%s", got)
	}
	if strings.Count(got, ">>> calvoproxy >>>") != 1 {
		t.Errorf("block written more than once:\n%s", got)
	}
}

// Invariant 6: --revert restores the original byte for byte.
func TestSetup_RevertRestoresOriginal(t *testing.T) {
	home := setupSandbox(t)
	config := filepath.Join(home, ".codex", "config.toml")
	original := "# original\nmodel = \"gpt-5\"\n"
	writeFile(t, config, original)

	runSetupWith([]string{"codex", "--apply"}, &strings.Builder{})
	if readFile(t, config) == original {
		t.Fatal("apply did not change the file, so revert proves nothing")
	}

	out := &strings.Builder{}
	if code := runSetupWith([]string{"codex", "--revert"}, out); code != 0 {
		t.Fatalf("revert exit = %d, want 0; output:\n%s", code, out)
	}
	if got := readFile(t, config); got != original {
		t.Errorf("revert did not restore the original:\ngot:\n%s\nwant:\n%s", got, original)
	}
}

// Invariant 7: Hermes stays read-only even under --apply. Its YAML is inspected
// with a line-wise heuristic, and a heuristic that reads must not write.
func TestSetup_HermesNeverWrites(t *testing.T) {
	home := setupSandbox(t)
	cfg := filepath.Join(home, ".hermes", "config.yaml")
	original := "model:\n  provider: openrouter\n"
	writeFile(t, cfg, original)
	t.Setenv("HERMES_HOME", filepath.Join(home, ".hermes"))

	out := &strings.Builder{}
	runSetupWith([]string{"hermes", "--apply", "--url", "http://127.0.0.1:8080"}, out)

	if got := readFile(t, cfg); got != original {
		t.Errorf("hermes config was modified:\n%s", got)
	}
	if !strings.Contains(out.String(), "custom_providers") {
		t.Errorf("hermes should print the block to paste; got:\n%s", out)
	}
}

// Invariant 8: an unknown tool fails clearly instead of panicking.
func TestSetup_UnknownToolFails(t *testing.T) {
	setupSandbox(t)
	out := &strings.Builder{}
	code := runSetupWith([]string{"inexistente"}, out)

	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "inexistente") {
		t.Errorf("error should name the unknown tool; got:\n%s", out)
	}
}

// Invariant 9: with nothing installed, report it — never create a config blind.
// Writing a settings.json for a tool the user does not have would be inventing
// state on their machine.
func TestSetup_MissingToolReportsWithoutCreating(t *testing.T) {
	home := setupSandbox(t)
	out := &strings.Builder{}
	code := runSetupWith([]string{"codex", "--apply"}, out)

	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("created a config for a tool that is not installed")
	}
	if code == 0 {
		t.Errorf("exit = 0, want non-zero when the tool is not found")
	}
	if !strings.Contains(strings.ToLower(out.String()), "no encontr") {
		t.Errorf("should say the tool was not found; got:\n%s", out)
	}
}

// --list names every integration so the surface is discoverable.
func TestSetup_ListNamesIntegrations(t *testing.T) {
	setupSandbox(t)
	out := &strings.Builder{}
	if code := runSetupWith([]string{"--list"}, out); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, name := range []string{"hermes", "claude-code", "codex"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("--list missing %q; got:\n%s", name, out)
		}
	}
}
