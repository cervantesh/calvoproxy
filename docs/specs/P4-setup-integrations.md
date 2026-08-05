# P4 — `calvoproxy setup <tool>`

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problem

`doctor` knows how to check that Hermes is wired correctly, but it **only checks**: when it
fails, it prints the block and leaves you to paste it. And it knows about Hermes only. The
same logic — find the install, know the right block, verify it took effect — applies to Claude
Code and Codex, the other two clients this proxy serves daily.

## 2. Contract

```go
type Integration interface {
    Name() string
    ConfigPath() string                       // "" when the tool is not found
    Render(baseURL string) string             // the block that must be present
    Current(path, baseURL string) state       // missing | stale | configured
    Apply(path, baseURL string) (backup string, err error)
    Verify(baseURL string) checkResult        // real round trip against the proxy
}
```

`Apply` returns the backup path so `--revert` knows what to restore.

```
calvoproxy setup <hermes|claude-code|codex> [--apply] [--revert] [--url URL]
calvoproxy setup --list
```

**`--check` is the default mode and writes nothing.** Writing into another program's file is
the only destructive operation in the whole plan, so the default reports and only `--apply`
touches disk.

## 3. Hard rules for writing

1. **Always back up before touching**, in `<config-dir>/calvoproxy/backups/<tool>-<ts>.bak`.
   `--revert` restores the most recent one.
2. **Never round-trip a parser over formats that carry comments.** The Codex TOML is patched as
   a marker-delimited block; only Claude Code's JSON — which has no comments — is read,
   modified and rewritten, and even then every other key is preserved.
3. **Idempotence.** Applying twice duplicates nothing, and the second time it reports that it
   was already configured.
4. **Hermes stays read-only.** Its YAML is inspected with a line-wise heuristic
   ([doctor.go:101](../../cmd/doctor.go)) and *a heuristic that reads must not write*: `Apply`
   prints the block and returns `errApplyNotSupported`. That is a decision, not a gap — it is
   in the interface so it can be seen.

## 4. Blocks per tool

**Claude Code** (`~/.claude/settings.json`) — speaks the Anthropic dialect against
`/v1/messages`, which the proxy already serves:

```json
{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8080", "ANTHROPIC_AUTH_TOKEN": "dummy"}}
```

**Codex** (`~/.codex/config.toml`) — an OpenAI-compatible provider:

```toml
# >>> calvoproxy >>>
model_provider = "calvoproxy"
[model_providers.calvoproxy]
name = "CalvoProxy"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "chat"
# <<< calvoproxy <<<
```

**Hermes** (`config.yaml`) — the block `hermesConfigBlock` already knows
([doctor.go:75](../../cmd/doctor.go)), printed for pasting.

## 5. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | `--check` never writes, neither with the file present nor absent | mtime and content untouched |
| 2 | `--apply` leaves a restorable backup | the backup exists and is byte-for-byte the original |
| 3 | `--apply` preserves unrelated JSON keys | settings with other keys; assert they are still there |
| 4 | `--apply` is idempotent | two passes; identical result and the second says "already configured" |
| 5 | The TOML keeps comments and prior content | config with comments; assert they survive |
| 6 | `--revert` restores the original byte for byte | apply then revert; exact comparison |
| 7 | Hermes never writes, not even with `--apply` | file untouched and the block in the output |
| 8 | Unknown tool → clear error, exit 2, no panic | `setup nonexistent` |
| 9 | With no config detected, it reports and does not create the file blind | empty HOME |

## 6. Out of scope

Cursor, Cline and Aider. The interface exists so they can be adapters, but the value of this
cut is validating the contract against three different formats (read-only YAML, JSON, TOML),
not covering the catalogue.
