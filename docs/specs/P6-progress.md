# P6 — progress

- **Invariants 1–7 (plus EOF and `/trace`): green.** Nine tests in
  `cmd/chat_test.go`; all three gates pass.
- **The spec was short on one point**: it did not say what happens to the history
  when a turn fails. If the upstream returns an error or the transport dies, the
  user's message is **withdrawn** from the history — leaving it in would make the
  next turn send a prompt no model ever saw, and the REPL would be lying about
  what it sent. Implemented that way; recorded here because it was not written
  down.
- **A second unspecified detail**: the `bufio.Scanner` buffer. The 64 KiB default
  silently truncates a pasted file, which is exactly what an operator will try.
  Raised to 8 MiB on input and 4 MiB for SSE parsing.
- For whoever comes next: `renderTrace` is the only piece that knows the header
  format. If P1's schema goes to `v2`, this is the place to touch.
