// Package cervoclaw provides CervoClaw/OpenClaw compatibility helpers.
package cervoclaw

import (
	"fmt"
	"os"

	configenv "github.com/cervantesh/cervo-config"
)

const (
	PrimaryPrefix = "CERVOCLAW_"
	LegacyPrefix  = "OPENCLAW_"

	DefaultConfigFilePath = "/etc/cervoclaw/config.json"
	DefaultSecretsDir     = "/var/secrets/cervoclaw"
)

// CloudLoaderOptions configures the CervoClaw cloud-ready loader preset.
type CloudLoaderOptions struct {
	// ConfigFilePath points to a flat JSON config file. Missing files are skipped.
	ConfigFilePath string
	// SecretsDir points to a mounted secret directory. Missing directories are skipped.
	SecretsDir string
	// Sources are checked before environment, config file, and secret directory sources.
	Sources []configenv.Source
	// DisableEnv removes os.LookupEnv from the source chain.
	DisableEnv bool
}

// Alias returns the OPENCLAW_* alias for a CERVOCLAW_* key.
func Alias(name string) string {
	return configenv.AliasByPrefix(name, PrimaryPrefix, LegacyPrefix)
}

// NewLoader returns a config loader with CervoClaw legacy prefix support.
func NewLoader() *configenv.Loader {
	return configenv.New(Options())
}

// NewCloudLoader returns a CervoClaw loader for cloud and agentic runtimes.
//
// Precedence is explicit sources, environment, optional JSON config file, then
// optional mounted secret directory. Missing default files/directories are
// ignored; existing unreadable or invalid sources return an error.
func NewCloudLoader(options CloudLoaderOptions) (*configenv.Loader, error) {
	configPath := options.ConfigFilePath
	if configPath == "" {
		configPath = DefaultConfigFilePath
	}
	secretsDir := options.SecretsDir
	if secretsDir == "" {
		secretsDir = DefaultSecretsDir
	}

	sources := append([]configenv.Source(nil), options.Sources...)
	if !options.DisableEnv {
		sources = append(sources, configenv.EnvSource())
	}
	if source, err := optionalJSONFileSource(configPath); err != nil {
		return nil, err
	} else if source != nil {
		sources = append(sources, source)
	}
	if source, err := optionalDirectorySource(secretsDir); err != nil {
		return nil, err
	} else if source != nil {
		sources = append(sources, source)
	}

	loaderOptions := Options()
	loaderOptions.Sources = sources
	return configenv.New(loaderOptions), nil
}

// NewCervoCloudLoader returns the standard CervoClaw cloud loader preset.
func NewCervoCloudLoader(options CloudLoaderOptions) (*configenv.Loader, error) {
	return NewCloudLoader(options)
}

// Options returns configenv options for CervoClaw legacy prefix support.
func Options() configenv.Options {
	return configenv.Options{
		PrefixAliases: []configenv.PrefixAlias{
			{PrimaryPrefix: PrimaryPrefix, AliasPrefix: LegacyPrefix},
		},
		WarnOnAlias: true,
	}
}

// String returns the configured CervoClaw value, checking OPENCLAW_* after the
// primary CERVOCLAW_* key.
func String(primary string, aliases ...string) string {
	return NewLoader().String(primary, aliases...)
}

func optionalJSONFileSource(path string) (configenv.Source, error) {
	source, err := configenv.JSONFileSource(path)
	if err == nil {
		return source, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("load CervoClaw config file %s: %w", path, err)
}

func optionalDirectorySource(path string) (configenv.Source, error) {
	source, err := configenv.DirectorySource(path)
	if err == nil {
		return source, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("load CervoClaw secret directory %s: %w", path, err)
}
