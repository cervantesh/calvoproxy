# P3 — Tool-result size guard

Reference architecture: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

> **This spec changed scope after being implemented.** The original version described two
> compression engines inside the proxy. They were in the wrong layer and moved to
> `github.com/cervantesh/cervo-compress`. What remains here is the only part that really is a
> proxy's business. The full reasoning is in §2, because the mistake is more instructive than
> the result.

## 1. What it does

Clips a tool result that exceeds `PROXY_TOOL_RESULT_LIMIT`, keeping **both ends** with an
explicit marker between them. **Off by default**: without that variable the proxy forwards
exactly what it received.

Same family as `PROXY_MAX_RESPONSE_BYTES`: a statement about what this proxy is willing to
carry, not a judgement about what the model needs.

## 2. Why the engines left

Deciding what a conversation may lose requires **knowing that conversation**: which tool result
still matters, what the user is doing, whether a block can be fetched back when the model asks
for it. The proxy sees a stateless snapshot and knows none of that. Hermes and the coding
agents do — they own the conversation.

A detail of OmniRoute confirms it: CCR, its only engine that genuinely **removes** content,
injects its retrieval protocol only when the caller exposes the `omniroute_ccr_retrieve` tool.
Not even they take context away without a contract with the client.

`dedup` left entirely. `toolcap` stayed, reframed: it is no longer "compress", it is "do not
carry half a megabyte in one message".

## 3. Rules

- **Only `role: "tool"` messages.** A user message is what was asked.
- **Never content that is valid JSON.** Truncating it yields invalid JSON, and a corrupt result
  is worse than a long one.
- **Never structured content** (block arrays: images, Anthropic-dialect `tool_result`). There
  is no safe generic way to clip it.
- **512-byte floor.** Below that, the marker would be most of what survives.
- **If the marker costs more than the cut saves**, nothing is touched.
- **On any panic**, the original body is forwarded and a warning logged. A failure here
  degrades to "not clipped", never to a 500.

## 4. Verifiable invariants

| # | Invariant | How it is tested |
|---|---|---|
| 1 | Off by default: the body comes out identical | byte-for-byte comparison |
| 2 | Never mutates the input map | copy kept and compared |
| 3 | Does not touch valid JSON | long JSON result |
| 4 | Keeps head and tail, and marks the cut | long non-JSON content |
| 5 | Does not touch messages other than `role: tool` | long user message |
| 6 | An absurd limit is clamped to the floor | `PROXY_TOOL_RESULT_LIMIT=1` |
| 7 | Odd shapes do not blow up | nil messages, mixed types |
| 8 | The clip reaches the trace as `cmp=` | header after clipping |
| 9 | Works through the router's real path, and off changes nothing | two integration tests |

## 5. Out of scope

Anything that is context management. It lives in
[`cervo-compress`](https://github.com/cervantesh/cervo-compress), as a library, so that
whoever owns the conversation can use it.
