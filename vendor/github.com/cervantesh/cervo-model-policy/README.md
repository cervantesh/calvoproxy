# CervoModelPolicy

CervoModelPolicy is a small Go library for deterministic model profile and
model-chain selection.

It is intentionally narrow. It is not a rules engine, proxy, HTTP client,
provider SDK, quota system, or authorization layer. It only answers:

- which profile is active
- which model should be tried first
- which fallback models should follow
- why that model chain was selected

Use it after your application has already decided that a request is allowed.

## Install

```bash
go get github.com/cervantesh/cervo-model-policy
```

```go
import cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
```

## Core Concepts

- `Profile`: a named model strategy such as `simple`, `coding`, `reasoning`,
  or `cheap`. Profiles map to ordered model chains.
- `ModelChain`: the ordered list of model attempts. The first item is the
  primary model. Every later item is a fallback.
- `Aliases`: user-facing names that resolve to profiles. Aliases are useful
  when a request uses a bare value such as `coding` in the model field and the
  application wants to treat that as profile selection.
- `RequestedModel`: a specific model requested by the caller. If it is empty or
  `auto`, the profile chain is used as-is. Otherwise, the requested model is
  prepended to the profile chain and duplicate entries are skipped.

## Quick Start

```go
package main

import (
	"fmt"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
)

func main() {
	policy := cervomodelpolicy.NewPolicy(cervomodelpolicy.Config{
		DefaultProfile: "simple",
		Profiles: map[string][]string{
			"simple": {"openai/gpt-4.1-mini", "openai/gpt-4.1-nano"},
			"coding": {"openai/gpt-4.1", "openai/gpt-4.1-mini"},
		},
		Aliases: map[string]string{
			"default": "simple",
			"fast":    "simple",
			"code":    "coding",
			"coding":  "coding",
		},
	})

	profile, requestedModel := policy.ResolveModelAlias("simple", "code")
	decision := policy.Select(cervomodelpolicy.Request{
		Profile:        profile,
		RequestedModel: requestedModel,
	})

	fmt.Println(decision.Profile)        // coding
	fmt.Println(decision.ModelChain)     // [openai/gpt-4.1 openai/gpt-4.1-mini]
	fmt.Println(decision.FallbackModels) // [openai/gpt-4.1-mini]
	fmt.Println(decision.Reason)         // model chain selected from profile coding
}
```

## Selection Behavior

### Auto Model

When `RequestedModel` is empty or `auto`, the selected profile chain is returned
unchanged.

```go
decision := policy.Select(cervomodelpolicy.Request{
	Profile:        "coding",
	RequestedModel: "auto",
})
```

If `coding` maps to `["model-a", "model-b"]`, then:

- `decision.ModelChain` is `["model-a", "model-b"]`
- `decision.FallbackModels` is `["model-b"]`

### Explicit Model

When `RequestedModel` contains a concrete model, that model is tried first.
The profile chain follows, with duplicate entries skipped.

```go
decision := policy.Select(cervomodelpolicy.Request{
	Profile:        "simple",
	RequestedModel: "model-b",
})
```

If `simple` maps to `["model-a", "model-b", "model-c"]`, then:

- `decision.ModelChain` is `["model-b", "model-a", "model-c"]`
- `decision.FallbackModels` is `["model-a", "model-c"]`

### Unknown Profile

Unknown, empty, or whitespace-only profiles fall back to `DefaultProfile`.
If no usable config is provided, `DefaultConfig()` is used.

### Alias Behavior

`ResolveModelAlias(profile, requestedModel)` treats a bare model value that
matches a configured alias as profile selection:

```go
profile, model := policy.ResolveModelAlias("simple", "coding")
// profile == "coding"
// model == "auto"
```

Provider/model strings such as `openai/gpt-4.1` pass through unchanged unless
the complete string is configured as an alias.

## Configuration Normalization

`NewPolicy` normalizes config before use:

- default profile names are lowercased and trimmed
- profile names are lowercased and trimmed
- model names are trimmed and otherwise preserved
- empty profile names are ignored
- empty model names are removed from chains
- profiles with no models after trimming are ignored
- aliases are lowercased and trimmed
- aliases pointing to missing profiles are ignored
- every profile is also registered as its own alias
- `default` always resolves to the active default profile

The policy stores normalized internal copies. Mutating the original input maps
or slices after construction does not change the policy. Use `policy.Config()`
to inspect a defensive copy of the normalized config.

## Runtime Configuration

`LoadConfig` can load a complete model policy from `cervo-config` sources.
Prefer the profile JSON variables for multi-profile deployments:

```env
CERVO_MODEL_DEFAULT_PROFILE=coding
CERVO_MODEL_PROFILES_JSON={"simple":["openai/gpt-4.1-mini"],"coding":["openai/gpt-4.1","openai/gpt-4.1-mini"]}
CERVO_MODEL_ALIASES_JSON={"default":"simple","fast":"simple","code":"coding"}
```

Legacy single-profile variables are still supported:

```env
CERVO_MODEL_DEFAULT=openai/gpt-4.1-mini
CERVO_MODEL_ALLOWED=openai/gpt-4.1-mini,openai/gpt-4.1-nano
CERVO_MODEL_POLICY_MODE=balanced
```

`LoadConfig` also returns `ValidationWarnings`, which come from
`ValidateConfig`. These warnings are non-fatal so applications can decide
whether to fail startup, log a warning, or fall back to defaults.

## Validation

Use `ValidateConfig` when loading policy from environment, files, dashboards, or
tenant config:

```go
issues := cervomodelpolicy.ValidateConfig(cfg)
for _, issue := range issues {
	log.Printf("model policy config warning: %s %s", issue.Code, issue.Message)
}
```

Validation reports actionable issues such as missing default profiles, empty
profile names, empty model names, duplicate models, long chains, empty alias
names, and aliases that point to missing profiles. `NormalizeConfig` remains
tolerant and deterministic; `ValidateConfig` is the stricter diagnostic layer.

## Integration Pattern

CervoModelPolicy should be called after request authorization and business
policy checks.

```go
if rulesDecision.Allow {
	profile, requestedModel := modelPolicy.ResolveModelAlias(
		selectedProfile,
		incomingRequestedModel,
	)

	modelDecision := modelPolicy.Select(cervomodelpolicy.Request{
		Profile:        profile,
		RequestedModel: requestedModel,
	})

	for _, model := range modelDecision.ModelChain {
		// Call your provider adapter or proxy with model.
	}
}
```

See [docs/INTEGRATION.md](docs/INTEGRATION.md) for a longer integration guide.

## API Summary

```go
func DefaultConfig() Config
func NormalizeConfig(cfg Config) Config
func ValidateConfig(cfg Config) []ValidationIssue
func NewPolicy(cfg Config) *Policy
func (p *Policy) Config() Config
func (p *Policy) Select(req Request) Decision
func (p *Policy) ResolveModelAlias(profile string, requestedModel string) (string, string)
func (p *Policy) ResolveProfileAlias(raw string) (string, bool)
```

## Development

Run tests with:

```bash
go test ./...
```

Format code with:

```bash
gofmt -w policy.go policy_test.go
```
