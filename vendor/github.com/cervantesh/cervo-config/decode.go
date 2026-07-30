package configenv

import (
	"encoding"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Field describes one configurable struct field.
type Field struct {
	Name        string
	Aliases     []string
	Default     string
	Required    bool
	Description string
	Separator   string
	Type        string
	Sensitive   bool
}

// Decode populates target from the default loader.
func Decode(target any) error {
	return defaultLoader.Decode(target)
}

// MustDecode populates target from the default loader and panics on error.
func MustDecode(target any) {
	defaultLoader.MustDecode(target)
}

// Describe returns configuration metadata from struct tags.
func Describe(target any) ([]Field, error) {
	return describeStruct(target)
}

// Decode populates target from the loader.
func (l *Loader) Decode(target any) error {
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("target must be a non-nil pointer to struct")
	}
	elem := value.Elem()
	if elem.Kind() != reflect.Struct {
		return errors.New("target must point to a struct")
	}
	fields, err := describeType(elem.Type())
	if err != nil {
		return err
	}
	for _, field := range fields {
		structField, ok := elem.Type().FieldByName(field.structName)
		if !ok {
			continue
		}
		targetField := elem.FieldByIndex(structField.Index)
		if !targetField.CanSet() {
			continue
		}
		raw := l.String(field.Name, field.Aliases...)
		if raw == "" {
			raw = field.Default
		}
		if raw == "" {
			if field.Required {
				return &MissingError{Key: field.Name}
			}
			continue
		}
		if err := setFieldValue(targetField, raw, field); err != nil {
			return err
		}
	}
	return nil
}

// MustDecode populates target from the loader and panics on error.
func (l *Loader) MustDecode(target any) {
	if err := l.Decode(target); err != nil {
		panic(fmt.Sprintf("decode configuration: %v", err))
	}
}

func describeStruct(target any) ([]Field, error) {
	value := reflect.ValueOf(target)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, errors.New("target must not be nil")
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return nil, errors.New("target must be a struct or pointer to struct")
	}
	described, err := describeType(value.Type())
	if err != nil {
		return nil, err
	}
	fields := make([]Field, len(described))
	for i, field := range described {
		fields[i] = field.Field
	}
	return fields, nil
}

type describedField struct {
	Field
	structName string
}

func describeType(t reflect.Type) ([]describedField, error) {
	fields := make([]describedField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		name := sf.Tag.Get("config")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		separator := sf.Tag.Get("sep")
		if separator == "" {
			separator = ","
		}
		fields = append(fields, describedField{
			Field: Field{
				Name:        name,
				Aliases:     splitTag(sf.Tag.Get("alias"), ","),
				Default:     sf.Tag.Get("default"),
				Required:    sf.Tag.Get("required") == "true",
				Description: sf.Tag.Get("desc"),
				Separator:   separator,
				Type:        configTypeName(sf.Type),
				Sensitive:   sf.Tag.Get("sensitive") == "true" || sf.Tag.Get("secret") == "true",
			},
			structName: sf.Name,
		})
	}
	return fields, nil
}

func splitTag(value, separator string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func setFieldValue(field reflect.Value, raw string, meta describedField) error {
	if field.CanAddr() {
		if unmarshaler, ok := field.Addr().Interface().(encoding.TextUnmarshaler); ok {
			if err := unmarshaler.UnmarshalText([]byte(raw)); err != nil {
				return &ParseError{Key: meta.Name, Value: raw, Type: field.Type().String(), Err: err}
			}
			return nil
		}
	}
	if field.Kind() == reflect.Pointer {
		if field.Type() == reflect.TypeOf(&url.URL{}) {
			parsed, err := parseURL(meta.Name, raw)
			if err != nil {
				return err
			}
			field.Set(reflect.ValueOf(parsed))
			return nil
		}
		return fmt.Errorf("%s: unsupported pointer type %s", meta.Name, field.Type())
	}
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		parsed, err := parseBool(meta.Name, raw)
		if err != nil {
			return err
		}
		field.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Type() == reflect.TypeOf(time.Duration(0)) {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return &ParseError{Key: meta.Name, Value: raw, Type: "duration", Err: err}
			}
			field.SetInt(int64(parsed))
			return nil
		}
		parsed, err := parseInt(meta.Name, raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(parsed))
	case reflect.Float32, reflect.Float64:
		parsed, err := parseFloat(meta.Name, raw)
		if err != nil {
			return err
		}
		field.SetFloat(parsed)
	case reflect.Slice:
		switch field.Type().Elem().Kind() {
		case reflect.String:
			values := splitTag(raw, meta.Separator)
			field.Set(reflect.ValueOf(values).Convert(field.Type()))
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			values, err := parseIntSlice(meta.Name, raw, meta.Separator, field.Type().Elem().Bits())
			if err != nil {
				return err
			}
			slice := reflect.MakeSlice(field.Type(), 0, len(values))
			for _, value := range values {
				item := reflect.New(field.Type().Elem()).Elem()
				item.SetInt(value)
				slice = reflect.Append(slice, item)
			}
			field.Set(slice)
		default:
			return fmt.Errorf("%s: unsupported slice type %s", meta.Name, field.Type())
		}
	case reflect.Map:
		if field.Type().Key().Kind() != reflect.String || field.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("%s: unsupported map type %s", meta.Name, field.Type())
		}
		values, err := parseStringMap(meta.Name, raw, meta.Separator)
		if err != nil {
			return err
		}
		mapValue := reflect.MakeMapWithSize(field.Type(), len(values))
		for key, value := range values {
			mapValue.SetMapIndex(reflect.ValueOf(key).Convert(field.Type().Key()), reflect.ValueOf(value).Convert(field.Type().Elem()))
		}
		field.Set(mapValue)
	default:
		return fmt.Errorf("%s: unsupported field type %s", meta.Name, field.Type())
	}
	return nil
}

func parseBool(key, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, &ParseError{Key: key, Value: raw, Type: "bool", Err: errors.New("expected 1, true, yes, on, 0, false, no, or off")}
	}
}

func parseInt(key, raw string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, &ParseError{Key: key, Value: raw, Type: "int", Err: err}
	}
	return parsed, nil
}

func parseIntSlice(key, raw, separator string, bitSize int) ([]int64, error) {
	parts := splitTag(raw, separator)
	values := make([]int64, 0, len(parts))
	for _, part := range parts {
		parsed, err := strconv.ParseInt(strings.TrimSpace(part), 10, bitSize)
		if err != nil {
			return nil, &ParseError{Key: key, Value: raw, Type: TypeIntSlice, Err: err}
		}
		values = append(values, parsed)
	}
	return values, nil
}

func parseFloat(key, raw string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, &ParseError{Key: key, Value: raw, Type: "float64", Err: err}
	}
	return parsed, nil
}

func parseStringMap(key, raw, separator string) (map[string]string, error) {
	entries := splitTag(raw, separator)
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		mapKey, mapValue, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, &ParseError{Key: key, Value: raw, Type: TypeStringMap, Err: errors.New("expected key=value entries")}
		}
		mapKey = strings.TrimSpace(mapKey)
		if mapKey == "" {
			return nil, &ParseError{Key: key, Value: raw, Type: TypeStringMap, Err: errors.New("expected non-empty map key")}
		}
		values[mapKey] = strings.TrimSpace(mapValue)
	}
	return values, nil
}

func parseURL(key, raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, &ParseError{Key: key, Value: raw, Type: "url", Err: err}
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, &ParseError{Key: key, Value: raw, Type: "url", Err: errors.New("expected absolute URL with scheme and host")}
	}
	return parsed, nil
}

func configTypeName(t reflect.Type) string {
	if t == reflect.TypeOf(time.Duration(0)) {
		return TypeDuration
	}
	if t == reflect.TypeOf(&url.URL{}) {
		return TypeURL
	}
	switch t.Kind() {
	case reflect.String:
		return TypeString
	case reflect.Bool:
		return TypeBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return TypeInt
	case reflect.Float32, reflect.Float64:
		return TypeFloat
	case reflect.Slice:
		switch t.Elem().Kind() {
		case reflect.String:
			return TypeStringSlice
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return TypeIntSlice
		default:
			return t.String()
		}
	case reflect.Map:
		if t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.String {
			return TypeStringMap
		}
		return t.String()
	default:
		if reflect.PointerTo(t).Implements(reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()) {
			return t.String()
		}
		return t.String()
	}
}
