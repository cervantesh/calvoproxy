package configenv

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrMissing marks a required configuration value as missing.
var ErrMissing = errors.New("configuration value is missing")

// LookupFunc resolves an environment key.
type LookupFunc func(key string) (string, bool)

// PrefixAlias describes a prefix migration rule.
type PrefixAlias struct {
	PrimaryPrefix string
	AliasPrefix   string
}

// Options configures a Loader.
type Options struct {
	Aliases        map[string][]string
	PrefixAliases  []PrefixAlias
	Sources        []Source
	Lookup         LookupFunc
	Logger         *slog.Logger
	WarnOnAlias    bool
	SplitSeparator string
}

// Loader reads environment-backed configuration values.
type Loader struct {
	aliases        map[string][]string
	prefixAliases  []PrefixAlias
	sources        []Source
	vars           []Var
	parsers        map[string]ParserFunc
	logger         *slog.Logger
	warnOnAlias    bool
	splitSeparator string
	warnedAliases  sync.Map
}

// ParseError describes a configured value that could not be parsed.
type ParseError struct {
	Key   string
	Value string
	Type  string
	Err   error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse %s from %s=%q: %v", e.Type, e.Key, e.Value, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

// MissingError describes a required key that has no configured value.
type MissingError struct {
	Key string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("%s: %v", e.Key, ErrMissing)
}

func (e *MissingError) Unwrap() error {
	return ErrMissing
}

// New returns a Loader configured with explicit aliases and prefix aliases.
func New(options Options) *Loader {
	sources := append([]Source(nil), options.Sources...)
	if options.Lookup != nil {
		sources = append(sources, SourceFunc(options.Lookup))
	}
	if len(sources) == 0 {
		sources = []Source{EnvSource()}
	}
	separator := options.SplitSeparator
	if separator == "" {
		separator = ","
	}
	logger := options.Logger
	if logger == nil && options.WarnOnAlias {
		logger = slog.Default()
	}

	aliases := make(map[string][]string, len(options.Aliases))
	for key, values := range options.Aliases {
		aliases[key] = append([]string(nil), values...)
	}

	return &Loader{
		aliases:        aliases,
		prefixAliases:  append([]PrefixAlias(nil), options.PrefixAliases...),
		sources:        sources,
		parsers:        make(map[string]ParserFunc),
		logger:         logger,
		warnOnAlias:    options.WarnOnAlias,
		splitSeparator: separator,
	}
}

func (l *Loader) durationFromRaw(key, raw string) (time.Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, &ParseError{Key: key, Value: raw, Type: "duration", Err: err}
	}
	return parsed, nil
}

// NewPrefixLoader returns a Loader with one prefix alias rule.
func NewPrefixLoader(primaryPrefix, aliasPrefix string) *Loader {
	return New(Options{
		PrefixAliases: []PrefixAlias{{PrimaryPrefix: primaryPrefix, AliasPrefix: aliasPrefix}},
	})
}

// String returns the first non-empty value from primary, configured aliases,
// prefix aliases, and explicit aliases.
func (l *Loader) String(primary string, aliases ...string) string {
	keys := append([]string{primary}, l.aliasesFor(primary, aliases...)...)
	for idx, key := range keys {
		if key == "" {
			continue
		}
		raw, ok := l.lookup(key)
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if idx > 0 {
			l.warnAliasOnce(primary, key)
		}
		return value
	}
	return ""
}

func (l *Loader) lookup(key string) (string, bool) {
	for _, source := range l.sources {
		if source == nil {
			continue
		}
		value, ok := source.Lookup(key)
		if ok {
			return value, true
		}
	}
	return "", false
}

// RequiredString returns a configured string or an error when it is missing.
func (l *Loader) RequiredString(primary string, aliases ...string) (string, error) {
	value := l.String(primary, aliases...)
	if value == "" {
		return "", &MissingError{Key: primary}
	}
	return value, nil
}

// StringDefault returns the configured string value or fallback.
func (l *Loader) StringDefault(primary, fallback string, aliases ...string) string {
	value := l.String(primary, aliases...)
	if value == "" {
		return fallback
	}
	return value
}

// Bool parses a boolean value.
func (l *Loader) Bool(primary string, aliases ...string) (bool, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, &ParseError{Key: primary, Value: value, Type: "bool", Err: errors.New("expected 1, true, yes, on, 0, false, no, or off")}
	}
}

// BoolDefault parses a boolean value or returns fallback.
func (l *Loader) BoolDefault(primary string, fallback bool, aliases ...string) bool {
	value, err := l.Bool(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

// Int parses an integer value.
func (l *Loader) Int(primary string, aliases ...string) (int, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, &ParseError{Key: primary, Value: value, Type: "int", Err: err}
	}
	return parsed, nil
}

// IntDefault parses an integer value or returns fallback.
func (l *Loader) IntDefault(primary string, fallback int, aliases ...string) int {
	value, err := l.Int(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

// Float parses a float64 value.
func (l *Loader) Float(primary string, aliases ...string) (float64, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, &ParseError{Key: primary, Value: value, Type: "float64", Err: err}
	}
	return parsed, nil
}

// FloatDefault parses a float64 value or returns fallback.
func (l *Loader) FloatDefault(primary string, fallback float64, aliases ...string) float64 {
	value, err := l.Float(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

// Duration parses a Go duration value.
func (l *Loader) Duration(primary string, aliases ...string) (time.Duration, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return 0, err
	}
	parsed, err := l.durationFromRaw(primary, value)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

// DurationDefault parses a Go duration value or returns fallback.
func (l *Loader) DurationDefault(primary string, fallback time.Duration, aliases ...string) time.Duration {
	value, err := l.Duration(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

// URL parses an absolute URL value.
func (l *Loader) URL(primary string, aliases ...string) (*url.URL, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, &ParseError{Key: primary, Value: value, Type: "url", Err: err}
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, &ParseError{Key: primary, Value: value, Type: "url", Err: errors.New("expected absolute URL with scheme and host")}
	}
	return parsed, nil
}

// URLDefault parses an absolute URL value or returns fallback.
func (l *Loader) URLDefault(primary string, fallback *url.URL, aliases ...string) *url.URL {
	value, err := l.URL(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

// StringSlice parses a separated string list, trimming items and dropping empty
// entries.
func (l *Loader) StringSlice(primary string, aliases ...string) ([]string, error) {
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(value, l.splitSeparator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			result = append(result, item)
		}
	}
	return result, nil
}

// StringSliceDefault parses a separated string list or returns fallback.
func (l *Loader) StringSliceDefault(primary string, fallback []string, aliases ...string) []string {
	value, err := l.StringSlice(primary, aliases...)
	if err != nil {
		return fallback
	}
	return value
}

func (l *Loader) aliasesFor(primary string, explicit ...string) []string {
	aliases := append([]string(nil), l.aliases[primary]...)
	for _, rule := range l.prefixAliases {
		aliases = append(aliases, AliasByPrefix(primary, rule.PrimaryPrefix, rule.AliasPrefix))
	}
	aliases = append(aliases, explicit...)
	return aliases
}

func (l *Loader) warnAliasOnce(primary, alias string) {
	if !l.warnOnAlias || l.logger == nil {
		return
	}
	key := primary + "|" + alias
	if _, loaded := l.warnedAliases.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	l.logger.Warn("using environment alias", slog.String("primary", primary), slog.String("alias", alias))
}
