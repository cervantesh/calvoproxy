# P6 — `calvoproxy chat`

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md). Consumes the trace from
[P1](P1-decision-trace.md).

## 1. Problem

Trying a chain today means either standing up Hermes or hand-writing `curl` with the body, and
`curl` does not decode the trace: it leaves you reading `v1;p=coding;s=0.83;a=2;prev=...` by
eye. What is needed is a diagnostic client of our own that talks to the proxy the way an agent
would and **shows the decision in plain words after every turn**.

It is also P1's dogfooding: if the trace cannot render as "served by X, skipped Y (breaker) and
Z (quota)", it is badly designed — and that is worth discovering before Hermes parses it.

## 2. Scope

It is a **client**. It does not import `internal/router` and reimplements no part of the chain:
it speaks HTTP to the proxy that is already running. No TUI framework: `bufio` over stdin and
ANSI codes. A real TUI would be thousands of vendored lines for a tool that competes with
`curl`.

```
calvoproxy chat [--profile coding] [--url http://127.0.0.1:8080] [--no-stream]
```

`--url` defaults to `proxyBaseURL()` ([cmd/doctor.go:274](../../cmd/doctor.go)), which already
honours the configured port.

## 3. Behaviour

- Loop: read a line from stdin, append it to the history as `user`, send **the whole** history
  (the upstream is stateless), print the deltas as they arrive, and append the reply as
  `assistant`.
- Streaming by default (`stream:true`), which is how agents use it. `--no-stream` for the
  non-streaming case.
- After each turn it prints the decoded trace on one line.
- Slash commands: `/profile <name>` switches profile, `/reset` clears the history, `/trace`
  toggles the full detail (sends `X-Calvoproxy-Trace: full`), `/quit` exits. EOF (Ctrl-D) is
  equivalent to `/quit`.
- An HTTP error is printed with its status and body, and the REPL **stays alive**: a 503 from
  an exhausted chain is information, not a reason to close.

### 3.1 Trace rendering

`v1;p=coding;s=0.83;a=2;n=4/4/3;prev=gpt-oss-20b:429;brk=1;cmp=off` prints as:

```
· coding · nemotron-3-super-120b-a12b · score 0.83 · attempt 2/3 · 1 excluded by breaker
  previously failed: gpt-oss-20b (429)
```

Rules: attempt 1 is not mentioned (it is the normal case); `brk=` only when > 0; the
`previously failed` line only when there is a `prev=`; `trunc=1` adds `(trace truncated)`.

## 4. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | Posts to the chosen profile's route, with the full history and `stream` matching the mode | test server capturing path and body |
| 2 | Prints SSE deltas in order and without the `data:` envelope | server emitting a known SSE stream |
| 3 | The trace decodes to readable text; with no header it invents nothing | pure render over sample headers, empty one included |
| 4 | An HTTP error is shown and the REPL survives the turn | server returning 503, followed by a good turn |
| 5 | The reply joins the history, so turn two sends three messages | two turns against the same server |
| 6 | `/profile`, `/reset` and `/quit` do their job; `/quit` and EOF exit with code 0 | scripted input with the commands |
| 7 | `--no-stream` uses the non-streaming path and extracts `choices[0].message.content` | server responding with non-SSE JSON |

## 5. Out of scope

History persisted between runs, line editing with history (arrow keys), and configurable
colours. It is a diagnostic tool, not a chat client.
