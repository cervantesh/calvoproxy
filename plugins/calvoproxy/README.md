# CalvoProxy plugin for Claude Code

Lets Claude Code query **free OpenRouter models** through the local CalvoProxy —
for second opinions, cheap/bulk subtasks, and drafts — without spending premium
tokens. Ships:

- the **`invoke-calvoproxy`** skill (auto-triggers on "ask the free model", "use
  calvoproxy", …), and
- the **`/ask-free <prompt>`** slash command.

## Install (minimal config)

From this repo (it doubles as a plugin marketplace):

```text
/plugin marketplace add cervantesh/calvoproxy      # or: /plugin marketplace add /path/to/cloned/calvoproxy
/plugin install calvoproxy@calvoproxy
```

Then make a CalvoProxy reachable — pick one:

- **Already running** (default `http://127.0.0.1:8080`): nothing else to do.
- **Start it yourself**: build once and run with your key:
  ```bash
  go build -o calvoproxy ./cmd
  OPENROUTER_API_KEY=sk-or-v1-... PROXY_IDLE_TIMEOUT=20m ./calvoproxy
  ```
- **Let the plugin start it on-demand**: set two env vars and the helper launches
  it when needed (and it self-stops after ~20 min idle):
  ```bash
  export CALVOPROXY_BIN=/path/to/calvoproxy        # or ...\calvoproxy.exe on Windows
  export OPENROUTER_API_KEY=sk-or-v1-...
  ```

A free OpenRouter API key comes from <https://openrouter.ai/keys>.

## Use

```text
/ask-free how would you structure a retry-with-jitter helper in Go?
```

or just ask naturally ("get a second opinion from the free model on this diff").
Optional env: `CALVOPROXY_URL` if the proxy runs on a non-default host/port.

## What it is / isn't

Free models (NVIDIA Nemotron-3, gpt-oss, Gemma, …) are weaker than your main
model — use them for offloading and contrast, not for authoritative or
correctness-critical answers. Prompts are sent to OpenRouter (external, but free);
don't include secrets.
