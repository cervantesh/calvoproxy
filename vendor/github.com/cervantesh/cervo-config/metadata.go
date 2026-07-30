package configenv

import (
	"errors"
	"fmt"
	"strings"
)

const (
	TypeString      = "string"
	TypeBool        = "bool"
	TypeInt         = "int"
	TypeFloat       = "float"
	TypeDuration    = "duration"
	TypeURL         = "url"
	TypeStringSlice = "[]string"
	TypeIntSlice    = "[]int"
	TypeStringMap   = "map[string]string"
)

// ParserFunc parses a raw string into a typed value.
type ParserFunc func(value string) (any, error)

// Var describes one configurable value.
type Var struct {
	Name        string
	Aliases     []string
	Default     string
	Required    bool
	Description string
	Separator   string
	Type        string
	Sensitive   bool
}

// ValidationError collects one or more configuration validation failures.
type ValidationError struct {
	Errors []error
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) Unwrap() []error {
	return e.Errors
}

// Register adds configuration metadata to the loader.
func (l *Loader) Register(vars ...Var) {
	for _, variable := range vars {
		if variable.Separator == "" {
			variable.Separator = l.splitSeparator
		}
		l.vars = append(l.vars, variable)
		if len(variable.Aliases) > 0 {
			l.aliases[variable.Name] = append(l.aliases[variable.Name], variable.Aliases...)
		}
	}
}

// Describe returns registered configuration metadata.
func (l *Loader) Describe() []Var {
	return append([]Var(nil), l.vars...)
}

// DescribeRedacted returns registered metadata with sensitive defaults redacted.
func (l *Loader) DescribeRedacted() []Var {
	return RedactVars(l.Describe())
}

// RegisterParser registers a custom parser by name.
func (l *Loader) RegisterParser(name string, parser ParserFunc) {
	if l.parsers == nil {
		l.parsers = make(map[string]ParserFunc)
	}
	l.parsers[name] = parser
}

// Parse parses a configured value with a registered parser.
func (l *Loader) Parse(parserName, primary string, aliases ...string) (any, error) {
	parser := l.parsers[parserName]
	if parser == nil {
		return nil, fmt.Errorf("parser %q is not registered", parserName)
	}
	value, err := l.RequiredString(primary, aliases...)
	if err != nil {
		return nil, err
	}
	parsed, err := parser(value)
	if err != nil {
		return nil, &ParseError{Key: primary, Value: value, Type: parserName, Err: err}
	}
	return parsed, nil
}

// Validate verifies all registered configuration values.
func (l *Loader) Validate() error {
	var validationErrors []error
	for _, variable := range l.vars {
		value := l.String(variable.Name, variable.Aliases...)
		if value == "" {
			value = variable.Default
		}
		if value == "" {
			if variable.Required {
				validationErrors = append(validationErrors, &MissingError{Key: variable.Name})
			}
			continue
		}
		if err := l.validateValue(variable, value); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}
	if len(validationErrors) > 0 {
		return &ValidationError{Errors: validationErrors}
	}
	return nil
}

func (l *Loader) validateValue(variable Var, value string) error {
	valueType := variable.Type
	if valueType == "" {
		valueType = TypeString
	}
	switch valueType {
	case TypeString:
		return nil
	case TypeBool:
		_, err := parseBool(variable.Name, value)
		return err
	case TypeInt:
		_, err := parseInt(variable.Name, value)
		return err
	case TypeFloat:
		_, err := parseFloat(variable.Name, value)
		return err
	case TypeDuration:
		_, err := l.durationFromRaw(variable.Name, value)
		return err
	case TypeURL:
		_, err := parseURL(variable.Name, value)
		return err
	case TypeStringSlice:
		return nil
	case TypeIntSlice:
		_, err := parseIntSlice(variable.Name, value, variable.Separator, 0)
		return err
	case TypeStringMap:
		_, err := parseStringMap(variable.Name, value, variable.Separator)
		return err
	default:
		if l.parsers[valueType] == nil {
			return fmt.Errorf("%s: parser %q is not registered", variable.Name, valueType)
		}
		_, err := l.Parse(valueType, variable.Name, variable.Aliases...)
		if err != nil && errors.Is(err, ErrMissing) && variable.Default != "" {
			_, parseErr := l.parsers[valueType](variable.Default)
			if parseErr != nil {
				return &ParseError{Key: variable.Name, Value: variable.Default, Type: valueType, Err: parseErr}
			}
			return nil
		}
		return err
	}
}
