---
name: invoke-cervoproxy
description: >-
  Query free LLMs (OpenRouter :free models) through the local CervoProxy for
  second opinions, cheap/bulk subtasks, drafts, or offloading work from the main
  model. Use when the user says "ask the free model", "use cervoproxy", "ask a
  free model", "second opinion from a free model", "offload this to the proxy",
  or when you want a throwaway/contrast answer without spending premium tokens.
license: MIT
---

# Invoke CervoProxy

Shell out to **CervoProxy** (`http://localhost:8080` by default) to get an answer
from a free OpenRouter model. The proxy applies a per-profile model chain and
fails over automatically across `:free` models, so you get an answer without a
premium API call.

Value to you is **offloading and contrast**, not authority: these are free
models, materially weaker than the model you run on. Use them for bulk/mechanical
work, first drafts, or an independent second read — **not** for critical
reasoning, final review, or anything security-sensitive. Sanity-check the output.

## Use

Invoke explicitly with `/skill:invoke-cervoproxy <question>`, or run the helper:

```bash
bash "${CLAUDE_PLUGIN_ROOT}/skills/invoke-cervoproxy/ask.sh" <profile> "<your prompt>"
```

- `<profile>` — `coding` (default), `reasoning`, `simple`, `creative`, `vision`.
  Pick by task: `coding` for code, `reasoning` for analysis, `simple` for short
  cheap answers.
- Prints the answer to stdout; the chosen model id goes to stderr.
- Pipe a long prompt: `some_command | ask.sh reasoning`.

## Config (minimal)

The only requirement is a reachable proxy. Optional env vars:

- `CERVOPROXY_URL` — proxy base URL (default `http://127.0.0.1:8080`). Set this
  if the proxy runs elsewhere.
- `CERVOPROXY_BIN` — path to the `cervoproxy` binary; if set, the helper starts
  the proxy on-demand when it's down (and it self-stops after ~20 min idle).
- `OPENROUTER_API_KEY` — only needed when the helper itself starts the proxy;
  a running proxy already holds its own key.

If the proxy isn't reachable and can't be started, the helper prints an `ERROR:`
line — fall back to answering directly; don't retry in a loop.

## When to use

Use it for: a free second opinion / contrast read on a diff or design, offloading
a bulk or mechanical subtask, a quick throwaway answer where premium quality
isn't worth it.

Skip it when: the task needs your full capability (hard reasoning, final review,
correctness-critical work), the user forbade external/network calls, or the
answer must be authoritative — the free models are not.

## Notes

- No key needed in the request: the real `OPENROUTER_API_KEY` lives in the proxy;
  the helper sends a dummy bearer token.
- It **is** an external network call (to OpenRouter) — free, but still leaves the
  machine. Don't send secrets or sensitive code in the prompt.
- Proxy source, setup, and editable model chains (`model-policy.json`) live in
  the CervoProxy repo this plugin ships with.
