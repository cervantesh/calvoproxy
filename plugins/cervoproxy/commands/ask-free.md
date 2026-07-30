---
description: Ask a free model via CervoProxy (second opinion / cheap subtask)
argument-hint: [profile] <prompt>
---

Run the CervoProxy helper to get an answer from a free model, then report it
concisely and make clear it came from a **free model via CervoProxy** (not your
own reasoning). If the first argument is one of `coding|reasoning|simple|creative|vision`,
use it as the profile; otherwise use `coding`.

```bash
bash "${CLAUDE_PLUGIN_ROOT}/skills/invoke-cervoproxy/ask.sh" coding "$ARGUMENTS"
```

If the helper prints an `ERROR:` line (proxy unreachable), tell the user how to
start CervoProxy (or set `CERVOPROXY_URL` / `CERVOPROXY_BIN`) and answer directly
instead. Do not retry in a loop.
