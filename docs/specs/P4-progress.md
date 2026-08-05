# P4 — progress

- **Invariants 1–9 green**, ten tests in `cmd/setup_test.go`. All three gates OK.
- **A real bug found by a test, not by review**: Go's `flag.Parse` stops at the
  first positional argument, so `setup codex --apply` parsed zero flags and
  behaved as `--check` — meaning `--apply` silently did nothing. Fixed with
  `splitToolAndFlags`, which separates the tool name from the flags in any order.
  Without invariant 5 this would have shipped broken.
- **The spec did not say where backups live on Windows.** `os.UserConfigDir()` is
  `APPDATA` there and `XDG_CONFIG_HOME` elsewhere; the test asks it rather than
  hardcoding one platform.
- Pending: Cursor, Cline and Aider as `Integration` adapters. The interface
  already covers the three formats (read-only YAML, JSON, TOML) that were the
  real risk.
