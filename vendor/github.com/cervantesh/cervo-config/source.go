package configenv

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Source resolves configuration keys from one backing store.
type Source interface {
	Lookup(key string) (string, bool)
}

// SourceFunc adapts a lookup function into a Source.
type SourceFunc func(key string) (string, bool)

func (f SourceFunc) Lookup(key string) (string, bool) {
	return f(key)
}

// MapSource resolves configuration keys from a map.
type MapSource map[string]string

func (m MapSource) Lookup(key string) (string, bool) {
	value, ok := m[key]
	return value, ok
}

type envSource struct{}

func (envSource) Lookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

// EnvSource returns a Source backed by os.LookupEnv.
func EnvSource() Source {
	return envSource{}
}

type flagSource struct {
	flags *flag.FlagSet
}

// FlagSource returns a Source backed by a flag.FlagSet.
func FlagSource(flags *flag.FlagSet) Source {
	return flagSource{flags: flags}
}

func (s flagSource) Lookup(key string) (string, bool) {
	if s.flags == nil {
		return "", false
	}
	found := s.flags.Lookup(key)
	if found == nil {
		return "", false
	}
	return found.Value.String(), true
}

// JSONFileSource loads a flat JSON object as a Source.
func JSONFileSource(path string) (Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	values := make(MapSource, len(raw))
	for key, value := range raw {
		if encoded, ok := scalarToString(value); ok {
			values[key] = encoded
		}
	}
	return values, nil
}

// DirectorySource loads files from a directory as key/value configuration.
// This matches mounted secret patterns used by Kubernetes and cloud platforms:
// file name is the key, file content is the value.
func DirectorySource(path string) (Source, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	values := make(MapSource, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(path, entry.Name())
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", fullPath, err)
		}
		values[entry.Name()] = strings.TrimSpace(string(data))
	}
	return values, nil
}

func scalarToString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
