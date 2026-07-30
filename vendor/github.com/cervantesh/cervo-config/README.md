# cervo-config

`cervo-config` is a zero-dependency Go configuration package for CervoClaw
services, cloud deployments, and agentic workers.

It focuses on operational configuration that commonly comes from environment
variables, flags, mounted secret files, JSON bootstrap files, or a project-owned
remote source adapter.

## Install

```bash
go get github.com/cervantesh/cervo-config
```

## Quick Start

```go
package main

import (
	"fmt"
	"time"

	configenv "github.com/cervantesh/cervo-config"
)

func main() {
	port := configenv.IntDefault("APP_PORT", 8080)
	timeout := configenv.DurationDefault("APP_TIMEOUT", 10*time.Second)
	enabled := configenv.BoolDefault("APP_ENABLED", true)

	fmt.Println(port, timeout, enabled)
}
```

## Multi-Source Loaders

Sources are checked in order. This keeps precedence explicit and predictable.

```go
fileSource, err := configenv.JSONFileSource("/etc/cervoclaw/config.json")
if err != nil {
	return err
}

secretSource, err := configenv.DirectorySource("/var/secrets/cervoclaw")
if err != nil {
	return err
}

loader := configenv.New(configenv.Options{
	Sources: []configenv.Source{
		configenv.EnvSource(),
		fileSource,
		secretSource,
	},
	PrefixAliases: []configenv.PrefixAlias{
		{PrimaryPrefix: "CERVOCLAW_", AliasPrefix: "OPENCLAW_"},
	},
	WarnOnAlias: true,
})
```

Built-in sources:

- `EnvSource()` for environment variables.
- `FlagSource(*flag.FlagSet)` for standard library flags.
- `MapSource` for tests, defaults, or embedded values.
- `JSONFileSource(path)` for flat JSON bootstrap config.
- `DirectorySource(path)` for mounted secret directories.
- `SourceFunc` for remote adapters such as Google Secret Manager or an agent
  control plane.

The core package does not import Google Cloud SDKs. Cloud-specific integrations
should adapt those clients through `Source` or `SourceFunc`.
See `examples/secretmanager_source_test.go` for a Google Secret Manager adapter
pattern that stays outside the core module dependencies.

## Struct Decoding

Use struct tags for service startup config:

```go
type Config struct {
	Port     int               `config:"CERVOCLAW_PORT" default:"8080" desc:"HTTP port"`
	Endpoint string            `config:"AGENT_ENDPOINT" required:"true"`
	Timeout  time.Duration     `config:"AGENT_TIMEOUT" default:"10s"`
	Scopes   []string          `config:"AGENT_SCOPES" default:"read,write" sep:","`
	Ports    []int             `config:"AGENT_PORTS" default:"8080,9090"`
	Labels   map[string]string `config:"AGENT_LABELS" default:"env=dev,team=agents"`
	Token    string            `config:"AGENT_TOKEN" required:"true" sensitive:"true"`
}

var cfg Config
if err := loader.Decode(&cfg); err != nil {
	return err
}

// For services that must fail fast during startup:
loader.MustDecode(&cfg)
```

Supported tags:

- `config:"NAME"` sets the key name.
- `alias:"OLD_NAME,OTHER_NAME"` sets fallback keys.
- `default:"value"` sets a default when no source provides a value.
- `required:"true"` fails when no value/default is configured.
- `desc:"text"` documents the field.
- `sep:","` sets the separator for slices and maps.
- `sensitive:"true"` marks defaults as secret for redacted docs and agent-safe
  metadata.

Supported field types:

- `string`
- `bool`
- integer types
- float types
- `time.Duration`
- `*url.URL`
- `[]string`
- `[]int`
- `map[string]string` as `key=value,key2=value2`

Use `Describe(&cfg)` to extract metadata for generated docs or agent
introspection. Use `RedactFields(fields)` or `MarkdownFields(fields)` when
metadata might be exposed to agents, dashboards, or logs.

For an explicit struct-decoding import path, use:

```go
import "github.com/cervantesh/cervo-config/structenv"
```

## Registered Metadata

Services can register configuration variables without structs:

```go
loader.Register(configenv.Var{
	Name:        "AGENT_ENDPOINT",
	Type:        configenv.TypeURL,
	Required:    true,
	Description: "Agent control-plane endpoint",
})

if err := loader.Validate(); err != nil {
	return err
}
```

Use `loader.Describe()` to generate operational docs or expose safe metadata to
agents.
Use `loader.DescribeRedacted()` or `loader.Markdown()` when defaults might
contain sensitive values.

```go
loader.Register(configenv.Var{
	Name:        "AGENT_TOKEN",
	Type:        configenv.TypeString,
	Required:    true,
	Sensitive:   true,
	Description: "Token used by the agent runtime",
})

docs := loader.Markdown()
```

## Custom Parsers

Register custom parsers for project-specific values:

```go
loader.RegisterParser("log_level", func(value string) (any, error) {
	switch value {
	case "debug", "info", "warn", "error":
		return value, nil
	default:
		return nil, fmt.Errorf("invalid log level")
	}
})

level, err := loader.Parse("log_level", "LOG_LEVEL")
```

Struct decoding also supports field types that implement
`encoding.TextUnmarshaler`.

## Strict Parsers

Default helpers return fallbacks when values are missing or invalid. Strict
parsers return errors and are better for startup validation:

```go
port, err := configenv.Int("APP_PORT")
if err != nil {
	return err
}
```

Strict parsers:

- `RequiredString`
- `Bool`
- `Int`
- `Float`
- `Duration`
- `URL`
- `StringSlice`

Default helpers:

- `StringDefault`
- `BoolDefault`
- `IntDefault`
- `FloatDefault`
- `DurationDefault`
- `URLDefault`
- `StringSliceDefault`

## CervoClaw Compatibility

CervoClaw/OpenClaw compatibility lives in the `cervoclaw` subpackage:

```go
import "github.com/cervantesh/cervo-config/cervoclaw"

tenantID := cervoclaw.String("CERVOCLAW_TENANT_ID")
loader := cervoclaw.NewLoader()
```

For Cloud Run, workers, agents, and local CervoClaw services, use the cloud
loader preset:

```go
import (
	configenv "github.com/cervantesh/cervo-config"
	"github.com/cervantesh/cervo-config/cervoclaw"
)

loader, err := cervoclaw.NewCloudLoader(cervoclaw.CloudLoaderOptions{
	Sources: []configenv.Source{
		configenv.SourceFunc(func(key string) (string, bool) {
			// Adapt a remote store, control plane, or Google Secret Manager
			// client here. The package does not import vendor SDKs.
			return "", false
		}),
	},
})
if err != nil {
	return err
}
```

The preset checks sources in this order:

1. Caller-provided sources.
2. Environment variables.
3. Optional flat JSON config at `/etc/cervoclaw/config.json`.
4. Optional mounted secrets under `/var/secrets/cervoclaw`.

The conventional file and secret paths are optional. Missing paths are skipped;
existing invalid config files or unreadable secret directories return an error.

The root package still includes `LegacyAlias` and `StringWithLegacy` as
deprecated compatibility wrappers.

## Development

```bash
go test ./...
go vet ./...
```

See `docs/maturity-roadmap.md` for comparison with mature Go configuration
libraries and the next implementation proposals.

Compatibility policy is documented in `docs/compatibility.md`.
