package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// `calvoproxy setup <tool>` wires a coding client to route through the proxy.
//
// doctor already knew how to CHECK Hermes, but only check: when it failed it
// printed the block and left you to paste it, and it knew about nothing else.
// The same shape — find the install, know the right block, prove it took effect
// — is what Claude Code and Codex need too.
//
// Writing into another program's configuration is the only destructive act in
// this feature, so three rules are absolute:
//
//  1. --check is the DEFAULT and never writes. Only --apply touches disk.
//  2. Every write is preceded by a byte-for-byte backup that --revert restores.
//  3. No parser round-trips on formats that carry comments. The Codex TOML is
//     patched as a marker-delimited block; only Claude Code's JSON — which has
//     no comments — is decoded and re-encoded, and even then every other key is
//     preserved.

var errApplyNotSupported = errors.New("esta integración es de solo lectura")

type integrationState int

const (
	stateMissing integrationState = iota // the tool's config was not found
	stateStale                           // found, but not pointing at the proxy
	stateConfigured
)

type Integration interface {
	Name() string
	ConfigPath() string
	Render(baseURL string) string
	Current(path, baseURL string) integrationState
	Apply(path, baseURL string) (backup string, err error)
}

func integrations() []Integration {
	return []Integration{hermesIntegration{}, claudeCodeIntegration{}, codexIntegration{}}
}

func runSetup(args []string) int { return runSetupWith(args, os.Stdout) }

// splitToolAndFlags separates the bare tool name from the flags, in any order,
// so `setup codex --apply` and `setup --apply codex` behave the same. Values
// that belong to --url are kept with the flags rather than mistaken for a tool.
func splitToolAndFlags(args []string) (tool string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// --url takes a value, which may arrive as a separate argument.
			if !strings.Contains(a, "=") && strings.TrimLeft(a, "-") == "url" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if tool == "" {
			tool = a
		}
	}
	return tool, flags
}

func runSetupWith(args []string, out io.Writer) int {
	// flag.Parse stops at the first non-flag argument, so `setup codex --apply`
	// would parse zero flags and silently behave as --check. Pull the tool name
	// out first and let the flag set see only flags.
	toolName, flagArgs := splitToolAndFlags(args)

	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	apply := fs.Bool("apply", false, "write the configuration (default: only report)")
	revert := fs.Bool("revert", false, "restore the most recent backup")
	list := fs.Bool("list", false, "list the supported tools")
	url := fs.String("url", "", "proxy base URL (default: the configured local proxy)")
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}

	if *list {
		fmt.Fprintln(out, "Integraciones disponibles:")
		for _, in := range integrations() {
			fmt.Fprintf(out, "  %-12s %s\n", in.Name(), describeState(in))
		}
		return 0
	}

	if toolName == "" {
		fmt.Fprintln(out, "uso: calvoproxy setup <hermes|claude-code|codex> [--apply] [--revert] [--url URL]")
		fmt.Fprintln(out, "     calvoproxy setup --list")
		return 2
	}

	name := toolName
	var target Integration
	for _, in := range integrations() {
		if in.Name() == name {
			target = in
			break
		}
	}
	if target == nil {
		names := make([]string, 0, 3)
		for _, in := range integrations() {
			names = append(names, in.Name())
		}
		fmt.Fprintf(out, "herramienta desconocida: %s\nconocidas: %s\n", name, strings.Join(names, ", "))
		return 2
	}

	base := strings.TrimRight(*url, "/")
	if base == "" {
		base = strings.TrimRight(proxyBaseURL(), "/")
	}

	if *revert {
		return revertIntegration(target, out)
	}

	path := target.ConfigPath()
	if path == "" {
		fmt.Fprintf(out, "[%s] no encontrado en este equipo.\n", target.Name())
		fmt.Fprintln(out, "       Instálalo primero; no se crea configuración a ciegas para una herramienta ausente.")
		return 1
	}

	fmt.Fprintf(out, "[%s] %s\n", target.Name(), path)

	switch target.Current(path, base) {
	case stateConfigured:
		fmt.Fprintln(out, "       ya configurado para este proxy — nada que hacer.")
		return 0
	case stateMissing:
		fmt.Fprintln(out, "       sin configurar para el proxy.")
	default:
		fmt.Fprintln(out, "       apunta a otro sitio; hay que actualizarlo.")
	}

	if !*apply {
		fmt.Fprintln(out, "\nBloque que se escribiría (usa --apply para hacerlo):")
		fmt.Fprintln(out)
		fmt.Fprintln(out, target.Render(base))
		return 0
	}

	backup, err := target.Apply(path, base)
	if errors.Is(err, errApplyNotSupported) {
		// Hermes on purpose: its YAML is read with a line-wise heuristic, and a
		// heuristic that reads must not write.
		fmt.Fprintln(out, "\n       Esta integración no se escribe automáticamente: su YAML se")
		fmt.Fprintln(out, "       inspecciona con una heurística, y una heurística que lee no debe")
		fmt.Fprintln(out, "       escribir. Pega este bloque a mano y reinicia la pasarela:")
		fmt.Fprintln(out)
		fmt.Fprintln(out, target.Render(base))
		return 0
	}
	if err != nil {
		fmt.Fprintf(out, "       no se pudo escribir: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "       escrito. Backup: %s\n", backup)
	fmt.Fprintf(out, "       revertir con: calvoproxy setup %s --revert\n", target.Name())
	fmt.Fprintln(out, "       reinicia la herramienta: ninguna recarga su config en caliente.")
	return 0
}

func describeState(in Integration) string {
	path := in.ConfigPath()
	if path == "" {
		return "(no encontrado)"
	}
	return path
}

// --- backups ---

func backupDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "calvoproxy", "backups")
}

// backupFile copies path aside before it is modified. The copy is byte for byte:
// --revert has to be able to undo the edit exactly, including whatever quirks
// the user's file had.
func backupFile(tool, path string) (string, error) {
	dir := backupDir()
	if dir == "" {
		return "", errors.New("no se pudo determinar el directorio de configuración")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s-%s.bak", tool, time.Now().UTC().Format("20060102T150405")))
	if err := os.WriteFile(dst, content, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

func latestBackup(tool string) string {
	dir := backupDir()
	if dir == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, tool+"-*.bak"))
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches) // timestamped names sort chronologically
	return matches[len(matches)-1]
}

func revertIntegration(target Integration, out io.Writer) int {
	backup := latestBackup(target.Name())
	if backup == "" {
		fmt.Fprintf(out, "[%s] no hay backup que restaurar.\n", target.Name())
		return 1
	}
	path := target.ConfigPath()
	if path == "" {
		fmt.Fprintf(out, "[%s] no encontrado; no hay dónde restaurar.\n", target.Name())
		return 1
	}
	content, err := os.ReadFile(backup)
	if err != nil {
		fmt.Fprintf(out, "       no se pudo leer el backup: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		fmt.Fprintf(out, "       no se pudo restaurar: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "[%s] restaurado desde %s\n", target.Name(), backup)
	return 0
}

// --- Hermes: read-only on purpose ---

type hermesIntegration struct{}

func (hermesIntegration) Name() string       { return "hermes" }
func (hermesIntegration) ConfigPath() string { return hermesConfigPath() }

func (hermesIntegration) Render(baseURL string) string {
	return hermesConfigBlock(baseURL + "/v1")
}

func (hermesIntegration) Current(path, baseURL string) integrationState {
	content, err := os.ReadFile(path)
	if err != nil {
		return stateMissing
	}
	lines := strings.Split(string(content), "\n")
	got := normBaseURL(yamlNestedScalar(lines, "model", "base_url"))
	switch {
	case got == "":
		return stateMissing
	case got == normBaseURL(baseURL+"/v1"):
		return stateConfigured
	default:
		return stateStale
	}
}

func (hermesIntegration) Apply(string, string) (string, error) {
	return "", errApplyNotSupported
}

// --- Claude Code: JSON, no comments, so a decode/encode round-trip is safe ---

type claudeCodeIntegration struct{}

func (claudeCodeIntegration) Name() string { return "claude-code" }

func (claudeCodeIntegration) ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	p := filepath.Join(home, ".claude", "settings.json")
	if fileExists(p) {
		return p
	}
	return ""
}

func (claudeCodeIntegration) Render(baseURL string) string {
	return fmt.Sprintf(`{
  "env": {
    "ANTHROPIC_BASE_URL": %q,
    "ANTHROPIC_AUTH_TOKEN": "dummy"
  }
}`, baseURL)
}

func (claudeCodeIntegration) Current(path, baseURL string) integrationState {
	content, err := os.ReadFile(path)
	if err != nil {
		return stateMissing
	}
	var parsed map[string]any
	if err := json.Unmarshal(content, &parsed); err != nil {
		return stateStale
	}
	env, _ := parsed["env"].(map[string]any)
	got, _ := env["ANTHROPIC_BASE_URL"].(string)
	switch {
	case got == "":
		return stateMissing
	case normBaseURL(got) == normBaseURL(baseURL):
		return stateConfigured
	default:
		return stateStale
	}
}

func (c claudeCodeIntegration) Apply(path, baseURL string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var parsed map[string]any
	if err := json.Unmarshal(content, &parsed); err != nil {
		return "", fmt.Errorf("el settings.json actual no es JSON válido: %w", err)
	}
	backup, err := backupFile(c.Name(), path)
	if err != nil {
		return "", err
	}
	// Merge, never replace: the user's other keys and other env vars are none of
	// our business and losing them is the failure this design exists to avoid.
	env, _ := parsed["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = baseURL
	env["ANTHROPIC_AUTH_TOKEN"] = "dummy"
	parsed["env"] = env

	encoded, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

// --- Codex: TOML with comments, so marker-delimited block only ---

type codexIntegration struct{}

const (
	codexBlockStart = "# >>> calvoproxy >>>"
	codexBlockEnd   = "# <<< calvoproxy <<<"
)

func (codexIntegration) Name() string { return "codex" }

func (codexIntegration) ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	p := filepath.Join(home, ".codex", "config.toml")
	if fileExists(p) {
		return p
	}
	return ""
}

func (codexIntegration) Render(baseURL string) string {
	return fmt.Sprintf(`%s
model_provider = "calvoproxy"

[model_providers.calvoproxy]
name = "CalvoProxy"
base_url = "%s/v1"
wire_api = "chat"
%s`, codexBlockStart, baseURL, codexBlockEnd)
}

func (c codexIntegration) Current(path, baseURL string) integrationState {
	content, err := os.ReadFile(path)
	if err != nil {
		return stateMissing
	}
	text := string(content)
	if !strings.Contains(text, codexBlockStart) {
		return stateMissing
	}
	if strings.Contains(text, `base_url = "`+baseURL+`/v1"`) {
		return stateConfigured
	}
	return stateStale
}

func (c codexIntegration) Apply(path, baseURL string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	backup, err := backupFile(c.Name(), path)
	if err != nil {
		return "", err
	}
	updated := replaceMarkedBlock(string(content), c.Render(baseURL))
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return backup, nil
}

// replaceMarkedBlock swaps our delimited block for a fresh one, or appends it.
// Everything outside the markers is copied verbatim — there is no vendored TOML
// parser, and a round-trip through one would silently eat the user's comments.
func replaceMarkedBlock(existing, block string) string {
	start := strings.Index(existing, codexBlockStart)
	end := strings.Index(existing, codexBlockEnd)
	if start >= 0 && end > start {
		tail := existing[end+len(codexBlockEnd):]
		return existing[:start] + block + tail
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	return existing + "\n" + block + "\n"
}
