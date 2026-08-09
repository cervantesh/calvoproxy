# Vendored dependencies

Most of this repo's dependencies are ordinary Go modules pinned by `go.sum`.
The locally replaced `github.com/cervantesh/cervo-*` modules are not, and
anyone auditing, forking, or trusting this proxy should know exactly how they
differ. `cervo-compress` is the exception: it is a public, checksummed module,
but its vendored copy is still covered by the manifest below.

## What the state actually is

`go.mod` declares each locally sourced module with a `replace` pointing into
`./third_party/`:

```
replace github.com/cervantesh/cervo-rules/v3 => ./third_party/cervo-rules
```

**`third_party/` is not in this repository.** The build succeeds anyway because
Go automatically selects `-mod=vendor` when `vendor/modules.txt` exists, and the
vendored trees under `vendor/github.com/cervantesh/` are complete. The
consequences are concrete and worth stating rather than discovering:

| | |
|---|---|
| `go build ./...` | works (vendor mode is auto-selected) |
| `go mod verify` | **fails** — it tries to read `third_party/<mod>/go.mod` |
| `go mod vendor` | **cannot regenerate** the tree; there is no source to vendor from |
| `go.sum` entries | **none** for these modules |
| upstream to diff against | **none reachable** from this repo |

So `vendor/github.com/cervantesh/` is not a cache of something authoritative.
It *is* the source. Editing a file there changes the shipped binary, and no Go
tooling would object.

## What compensates for it

`scripts/vendor-manifest.sha256` records a SHA-256 for every vendored `cervo-*`
source file, and CI verifies it on every push and pull request.

```bash
./scripts/vendor-manifest.sh          # verify (what CI runs)
./scripts/vendor-manifest.sh --update # re-record, then review the diff
```

Be precise about what this buys: it establishes **integrity**, not
**provenance**. It cannot prove these files are what some upstream published —
there is no upstream to compare against. It proves only that they have not
changed since they were recorded, and it forces any change to appear as a
reviewable line in a diff. Given that this proxy handles API keys and every
request body that passes through it, silent drift there is exactly the kind of
change that should be loud.

## The modules

| Module | Files | What it does here |
|---|---|---|
| `cervo-compress` | 8 | Public, checksummed tool-result preprocessing used by the local client bridge; no network or model calls. |
| `cervo-rules/v3` | 21 | Policy engine: the allow/deny decision, operation targets, limits. Every request passes through it. |
| `cervo-config` | 9 | Configuration loading and the CalvoProxy-specific config shape. |
| `cervo-httpkit` | 5 | HTTP transport helpers, including the global (host-level) breaker transport. |
| `cervo-model-policy` | 4 | Parsing and validating `model-policy.json` — profiles, chains, capabilities. |
| `cervo-observe` | 3 | Tracing and metric plumbing. |
| `cervo-requestmeta` | 3 | Request metadata extraction (user, capability headers). |
| `cervo-contracts` | 2 | Shared proxy protobuf/interface contracts. |
| `cervo-retry` | 2 | Retry classification and backoff — `ShouldRetry`, `RetryBackoff`, transport error classification. |

Two of these carry known sharp edges that this repo works around rather than
patches, documented at the call sites:

- **`cervo-retry`'s `ClassifyTransportError`** marks everything
  `BreakerEligible` by default and has no `context.Canceled` branch. Left
  alone, a handful of client disconnects opens the circuit for every model.
  `internal/router/router_upstream.go` intercepts cancellation before
  classification.
- **`cervo-httpkit`'s global breaker** counts *any* transport error against the
  shared host. `internal/router/router_breaker.go` makes cancellation neutral
  there for the same reason.

## If you are forking this

The honest options, in order of preference:

1. Obtain the `cervo-*` sources and populate `third_party/`, restoring
   `go mod verify` and `go mod vendor`.
2. Replace them with public equivalents. `cervo-retry` and `cervo-httpkit` are
   small and largely reimplementable; `cervo-rules` is not.
3. Keep the vendored trees and rely on the manifest, understanding that you are
   trusting this repository's history rather than a module checksum database.
