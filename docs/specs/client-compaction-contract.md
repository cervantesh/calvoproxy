# Client compaction contract v1

CalvoProxy routes requests and publishes capacity hints. It does **not** own a
conversation, decide what history may be forgotten, call a summarizer on behalf
of a client, or persist a transcript. Pi, Hermes, and future clients own those
decisions.

This is the small coordination contract shared with the Pi integration. It is
optional and add-only: an older client or proxy that knows none of it continues
to work.

## Request metadata

| Header | Value | Semantics |
|---|---|---|
| `X-Calvoproxy-Session-Id` | Opaque client-generated 128-bit identifier | Stable for one conversation only. Existing CalvoProxy affinity stores a process-local HMAC, never the raw value. It must not encode a user, account, channel, workspace, prompt, or path. |
| `X-Calvoproxy-Compaction` | `v1;g=<uint>;cause=<enum>;result=<enum>;tool=<enum>` | Optional content-free notice about the latest client compaction. CalvoProxy may use it for aggregate telemetry but must not reconstruct or store conversation state. |

Closed v1 values are:

- `cause`: `threshold`, `growth`, `tools`, `emergency`, or `manual`;
- `result`: `structured` or `native`;
- `tool`: `cervo` or `none`;
- `g`: a session-local generation counter starting at 1.

Clients never put summaries, prompts, token counts, filenames, tool output,
user/channel identifiers, errors, or free-form text in either header. Unknown
fields are ignored. If a host has no safe per-request header hook, it omits the
compaction header; it must not smuggle the metadata into the prompt.

## Response hints clients consume

- `Retry-After`: integer seconds on `429`/`503`; this is the minimum wait before
  retrying. Hermes already honors this in its native outer retry loop.
- `X-Calvoproxy-Route`: the existing versioned decision trace. `q=<n>` is the
  count of quota-excluded routes. `cmp=` describes only CalvoProxy's transport
  size guard; it never means that the client conversation was compacted.
- `X-Calvoproxy-Model`, `X-Calvoproxy-Profile`, and
  `X-Calvoproxy-Attempt`: the route that served the request and whether fallback
  was required.

Clients must tolerate every hint being absent. They do not derive deletion or
summary policy from proxy headers. A quota hint can delay a retry; it cannot
authorize discarding more conversation history.

## Local tool-result bridge

Hermes uses `calvoproxy compact-tools` locally before semantic compaction. The
stdin/stdout protocol is `calvoproxy.tool-compression.v1`:

```json
{"version":"calvoproxy.tool-compression.v1","messages":[],"tool_result_limit":4096}
```

The response returns the same message envelope plus a content-free byte-saving
report. The command calls `cervo-compress` in-process and performs no network
I/O. Failure, timeout, an unknown protocol, or an invalid envelope means
"preserve the original messages". This local bridge is not a proxy endpoint and
does not weaken the stateless wire boundary above.

## Hermes integration

Install the directory
`integrations/hermes/context_engine/calvoproxy` as
`plugins/context_engine/calvoproxy` in the Hermes checkout, then select it:

```yaml
context:
  engine: calvoproxy
```

Set `CALVOPROXY_BIN` to the CalvoProxy executable when it is not on `PATH`.
The integration:

1. triggers proactive native Hermes compaction at 12,000 tokens by default;
2. prevents overlapping compactions and applies a 60-second success cooldown;
3. preprocesses only older tool results through `cervo-compress`, protecting
   the recent tail and preserving the original on every bridge failure;
4. maps Hermes' redacted native checkpoint to the validated fields
   `Objective`, `Progress`, `Constraints`, `Files`, `Blockers`, and
   `Next Action`;
5. commits the untouched Hermes-native summary if that mapping is incomplete.

The defaults can be tuned with `CALVOPROXY_COMPACTION_TRIGGER_TOKENS`,
`CALVOPROXY_COMPACTION_COOLDOWN_SECONDS`,
`CALVOPROXY_TOOL_PRUNE_TRIGGER_TOKENS`, and
`CALVOPROXY_TOOL_RESULT_LIMIT`.
