# CalvoProxy Policy Integration

CalvoProxy uses CervoRules v3 for deterministic request policy decisions and
CervoModelPolicy for model profile/model-chain selection.

## CervoRules v3 Usage

CalvoProxy imports the v3 modular packages directly:

- `github.com/cervantesh/cervo-rules/v3/core`
- `github.com/cervantesh/cervo-rules/v3/runtime`

These dependencies are vendored (`vendor/`), so no external checkout or network
access is needed to build. The module identity is
`github.com/cervantesh/cervo-rules/v3`.

Generated vocabulary uses CervoRules v3 primitives:

- `Operation`: what the request is trying to do.
- `Target`: the logical backend selected by policy.
- `Executor`: the execution/provider choice selected by policy.

CalvoProxy stays responsible for gateway operations that are intentionally not
part of the v3 core runtime:

- HTTP request parsing and body-derived facts.
- timeout, retry and circuit-breaker behavior.
- limit enforcement and HTTP status mapping.
- audit classification.
- upstream forwarding and response handling.
- model/provider attempt execution.

## Runtime Flow

```text
HTTP request
  -> CalvoProxy HTTP adapter extracts request facts
  -> CervoRules v3 evaluates Operation -> Target/Executor
  -> CalvoProxy enriches the v3 decision with gateway operational policy
  -> CalvoProxy denies or continues
  -> CervoModelPolicy selects the model chain
  -> CalvoProxy executes provider/model attempts
```

The hot path calls `Engine.DecideWithOptions` with trace and observation
disabled. CervoRules v3 still owns the allow/deny, target, executor and fallback
executor decision. CalvoProxy owns operational metadata around that decision.

## Policy Construction

Policy is declarative and generated from:

- `internal/router/policyvocab/policy-vocabulary.yaml`
- `internal/router/policyrules/policy-rules.yaml`

Generated Go lives in:

- `internal/router/policyvocab/generated.go`
- `internal/router/policyrules/generated_policy.go`
- `internal/router/policyrules/generated_policy_test.go`

The generated entrypoint is:

```go
factory := policyrules.NewPolicyFactory()
cfg := factory.DefaultConfig()
err := factory.ValidateConfig(cfg)
engine, err := factory.Build(context.Background(), cfg)
metadata := factory.Metadata()
```

`internal/router/router_policy_config.go` loads startup environment into a local
`ruleRuntimeConfig`. The embedded `runtime.PolicyRuntimeConfig` is passed to the
generated v3 policy factory. Gateway-only fields such as retry, breaker, limits
and timeout remain local to CalvoProxy.

## Runtime Overrides

Supported overrides:

- `CERVO_MODEL_DEFAULT_PROFILE`: preferred model-policy default profile.
- `CERVO_MODEL_PROFILES_JSON`: preferred JSON object mapping model profiles to
  ordered model chains.
- `CERVO_MODEL_ALIASES_JSON`: preferred JSON object mapping aliases to model
  profiles.
- `CERVO_MODEL_DEFAULT`, `CERVO_MODEL_ALLOWED`, `CERVO_MODEL_POLICY_MODE`:
  legacy single-profile model-policy variables supported by CervoModelPolicy.
- `CERVO_MODEL_POLICY_STRICT`: when truthy, model-policy validation warnings
  make the proxy not ready and force deny-all policy decisions.
- `PROXY_DEFAULT_PROVIDER`: kept for environment compatibility, mapped to v3
  `DefaultExecutor`.
- `PROXY_PROVIDER_FALLBACKS_JSON`: kept for environment compatibility, mapped
  to v3 executor fallbacks.
- `PROXY_DEFAULT_PROFILE`: compatibility override for the model-policy default
  profile.
- `PROXY_PROVIDER_PROFILES_JSON`: compatibility override for model profiles.
- `PROXY_PROVIDER_ALIASES_JSON`: compatibility override for profile aliases.
- `PROXY_PLANNING_SERVICE`: maps planning operation to a target override.
- `PROXY_MEDIA_SERVICE`: enables the media route through a target override.
- `PROXY_LIMITS_JSON`
- `PROXY_MAX_BODY_BYTES`
- `PROXY_RETRY_POLICY_JSON`
- `PROXY_TRUSTED_USERS`

Generated CervoRules v3 policy validates operation, target and executor
vocabulary. CalvoProxy validates and applies gateway-owned runtime behavior.

Model policy precedence is:

```text
internal/router/config/model-policy.default.json
  -> CERVO_MODEL_* variables
  -> PROXY_PROVIDER_PROFILES_JSON / PROXY_PROVIDER_ALIASES_JSON
  -> PROXY_DEFAULT_PROFILE
  -> CervoModelPolicy normalization
```

The `PROXY_*` model variables are kept as later overrides for existing
deployments. New deployments should prefer `CERVO_MODEL_*`.

## Structured Policy Errors

CalvoProxy treats CervoRules errors as operational diagnostics, not response
payloads. Generated policy build failures now surface as
`runtime.PolicyBuildError` with policy metadata plus structured `core.Errors`.

Startup behavior:

- invalid generated policy config logs `error_kind=policy_build`;
- logs include policy name, DSL version, policy hash and vocabulary hash;
- logs include stable error codes and field paths such as
  `operation_targets[planning].target`;
- CalvoProxy falls back to a deny-all policy engine when startup policy build is
  not trustworthy.

Request-time behavior:

- `Decision.Allow=false` remains a normal policy denial, not an internal error;
- `err != nil` from the CervoRules engine means policy evaluation is not
  trustworthy and maps to a generic `500` response;
- structured error values are redacted before logging;
- HTTP clients receive generic policy failure text, never raw rule metadata,
  request body, prompt, authorization header or token-like values.

## Policy Telemetry

Runtime policy evaluation emits one versioned telemetry event:

```text
event=cervorules.policy.decision
schema_version=calvoproxy.policy_telemetry.v1
```

Logs, metrics and traces share the same low-cardinality policy fields:

- `policy_name`
- `policy_hash`
- `operation`
- `target`
- `executor`
- `decision`: `allow`, `deny` or `error`
- `rule_id`
- `audit_class`
- `requires_audit`
- `error_codes`
- `error_fields`
- `duration_ms`

Metrics are intentionally stricter than logs. Metric labels never include
`request_id`, `user`, `reason`, prompt/body/content, auth headers, token-like
values, model names, profile names or arbitrary metadata.

OpenTelemetry uses a lightweight span named `calvoproxy.policy.evaluate`.
The span is marked as error only when the CervoRules engine returns `err != nil`.
Policy denials are normal business decisions and do not mark the span as error.

Operational controls:

- `CERVO_POLICY_LOG_ENABLED`: defaults to `true`.
- `CERVO_POLICY_LOG_LEVEL`: `info`, `warn` or `error`; defaults to `info`.
- `CERVO_POLICY_TRACE_ENABLED`: defaults to `true` for lightweight policy spans.
- `CERVO_POLICY_METRICS_ENABLED`: defaults to `true`.
- `CERVO_POLICY_OBSERVATION_SAMPLE_RATE`: defaults to `0`, so CervoRules
  trace/observation materialization stays off in the hot path.
- `CERVO_POLICY_DEBUG_INCLUDE_TRACE`: defaults to `false`; when enabled it opts
  into full CervoRules trace/observation data for diagnostics.

Before merging policy changes, run:

```bash
bash ../scripts/check_calvoproxy_policy.sh
```

The check validates policy YAML and fails if generated policy files are stale.

## Operational Metadata

Health output includes generated policy metadata:

- `policy_name`
- `policy_dsl_version`
- `policy_hash`
- `policy_vocabulary_hash`
- `model_policy`

This lets machines verify which generated CervoRules policy is active without
parsing source code.

`GET /health/model-policy` returns only the model-policy snapshot:

- active default profile
- profile names
- alias names
- strict-mode state
- validation warnings, when present

Policy decision logs emit low-cardinality CervoRules fields:

- `operation`
- `target`
- `executor`
- `reason`
- `rule_id`
- `audit_class`
- `proxy_ready`
- `proxy_status`

Request profile, requested model and body-derived values remain out of
low-cardinality observability fields.

## Model Selection

CervoModelPolicy owns model selection:

- default profile
- profile aliases
- `auto` model behavior
- explicit requested model prepending
- fallback model chain

CalvoProxy calls:

```go
routerService.activeModelPolicy().Select(...)
```

This happens only after CervoRules has allowed the request.

At startup, CalvoProxy loads model policy through CervoModelPolicy-compatible
runtime configuration. Prefer:

```env
CERVO_MODEL_DEFAULT_PROFILE=simple
CERVO_MODEL_PROFILES_JSON={"simple":["openrouter/free"],"coding":["qwen/qwen3-coder:free","openrouter/free"]}
CERVO_MODEL_ALIASES_JSON={"default":"simple","code":"coding"}
```

The older `PROXY_PROVIDER_PROFILES_JSON`, `PROXY_PROVIDER_ALIASES_JSON`, and
`PROXY_DEFAULT_PROFILE` variables remain supported and are applied after
`CERVO_MODEL_*` values so existing deployments keep their override behavior.

Built-in defaults live in:

```text
internal/router/config/model-policy.default.json
```

Update that file when changing the built-in model roster. Runtime overrides
should use environment variables instead of code changes.

Preserved behavior:

- `calvoproxy/<profile>` selects a profile.
- `calvoproxy/<profile>` selects a profile.
- bare aliases such as `coding` select a profile.
- `auto` uses the selected profile chain.
- explicit model names are tried before profile fallback models.
- image content forces `vision` when that profile exists.
