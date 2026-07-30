package configenv

import (
	"net/url"
	"strings"
	"time"
)

var defaultLoader = New(Options{WarnOnAlias: true})

// AliasByPrefix returns an alias by replacing primaryPrefix with aliasPrefix.
// It returns an empty string when name does not start with primaryPrefix.
func AliasByPrefix(name, primaryPrefix, aliasPrefix string) string {
	if strings.HasPrefix(name, primaryPrefix) {
		return aliasPrefix + strings.TrimPrefix(name, primaryPrefix)
	}
	return ""
}

// LegacyAlias returns a backward-compatible legacy env name.
// Example: CERVOCLAW_FOO -> OPENCLAW_FOO.
//
// Deprecated: use AliasByPrefix(name, "CERVOCLAW_", "OPENCLAW_") in CervoClaw
// projects, or pass explicit aliases to String and default helpers.
func LegacyAlias(name string) string {
	return AliasByPrefix(name, "CERVOCLAW_", "OPENCLAW_")
}

// String returns the first non-empty environment value from primary, then aliases.
// Values are trimmed before they are checked and returned. When an alias is
// used, a legacy-key warning is logged once for the primary/alias pair.
func String(primary string, aliases ...string) string {
	return defaultLoader.String(primary, aliases...)
}

// StringWithLegacy checks primary first and then its auto-computed legacy alias.
//
// Deprecated: use String(primary, aliases...) with explicit aliases.
func StringWithLegacy(primary string) string {
	legacy := LegacyAlias(primary)
	if legacy == "" {
		return String(primary)
	}
	return String(primary, legacy)
}

// StringDefault returns the configured string value or fallback when the
// primary key and aliases are missing or empty.
func StringDefault(primary, fallback string, aliases ...string) string {
	return defaultLoader.StringDefault(primary, fallback, aliases...)
}

// RequiredString returns the configured string value or an error when the
// primary key and aliases are missing or empty.
func RequiredString(primary string, aliases ...string) (string, error) {
	return defaultLoader.RequiredString(primary, aliases...)
}

// Bool parses a boolean value from the primary key or aliases.
func Bool(primary string, aliases ...string) (bool, error) {
	return defaultLoader.Bool(primary, aliases...)
}

// BoolDefault parses a boolean value from the primary key or aliases.
// Accepted true values are 1, true, yes, and on. Accepted false values are 0,
// false, no, and off. Empty or invalid values return fallback.
func BoolDefault(primary string, fallback bool, aliases ...string) bool {
	return defaultLoader.BoolDefault(primary, fallback, aliases...)
}

// Int parses an integer value from the primary key or aliases.
func Int(primary string, aliases ...string) (int, error) {
	return defaultLoader.Int(primary, aliases...)
}

// IntDefault parses an integer value from the primary key or aliases. Empty or
// invalid values return fallback.
func IntDefault(primary string, fallback int, aliases ...string) int {
	return defaultLoader.IntDefault(primary, fallback, aliases...)
}

// Float parses a float64 value from the primary key or aliases.
func Float(primary string, aliases ...string) (float64, error) {
	return defaultLoader.Float(primary, aliases...)
}

// FloatDefault parses a float64 value from the primary key or aliases. Empty or
// invalid values return fallback.
func FloatDefault(primary string, fallback float64, aliases ...string) float64 {
	return defaultLoader.FloatDefault(primary, fallback, aliases...)
}

// Duration parses a Go duration from the primary key or aliases.
func Duration(primary string, aliases ...string) (time.Duration, error) {
	return defaultLoader.Duration(primary, aliases...)
}

// DurationDefault parses a Go duration from the primary key or aliases. Empty
// or invalid values return fallback.
func DurationDefault(primary string, fallback time.Duration, aliases ...string) time.Duration {
	return defaultLoader.DurationDefault(primary, fallback, aliases...)
}

// URL parses an absolute URL from the primary key or aliases.
func URL(primary string, aliases ...string) (*url.URL, error) {
	return defaultLoader.URL(primary, aliases...)
}

// URLDefault parses an absolute URL from the primary key or aliases. Empty or
// invalid values return fallback.
func URLDefault(primary string, fallback *url.URL, aliases ...string) *url.URL {
	return defaultLoader.URLDefault(primary, fallback, aliases...)
}

// StringSlice parses a separated string list from the primary key or aliases.
func StringSlice(primary string, aliases ...string) ([]string, error) {
	return defaultLoader.StringSlice(primary, aliases...)
}

// StringSliceDefault parses a separated string list from the primary key or
// aliases. Empty values return fallback.
func StringSliceDefault(primary string, fallback []string, aliases ...string) []string {
	return defaultLoader.StringSliceDefault(primary, fallback, aliases...)
}
