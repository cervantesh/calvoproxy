package configenv

import (
	"strings"
)

// MarkdownVars renders registered configuration metadata as a Markdown table.
func MarkdownVars(vars []Var) string {
	var builder strings.Builder
	builder.WriteString("| Name | Type | Required | Default | Description | Aliases | Sensitive |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, variable := range vars {
		builder.WriteString("| ")
		builder.WriteString(markdownCell(variable.Name))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(defaultString(variable.Type, TypeString)))
		builder.WriteString(" | ")
		builder.WriteString(markdownBool(variable.Required))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(redactedDefault(variable.Default, variable.Sensitive)))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(variable.Description))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(strings.Join(variable.Aliases, ", ")))
		builder.WriteString(" | ")
		builder.WriteString(markdownBool(variable.Sensitive))
		builder.WriteString(" |\n")
	}
	return builder.String()
}

// MarkdownFields renders struct tag configuration metadata as a Markdown table.
func MarkdownFields(fields []Field) string {
	var builder strings.Builder
	builder.WriteString("| Name | Type | Required | Default | Description | Aliases | Sensitive |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, field := range fields {
		builder.WriteString("| ")
		builder.WriteString(markdownCell(field.Name))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(defaultString(field.Type, TypeString)))
		builder.WriteString(" | ")
		builder.WriteString(markdownBool(field.Required))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(redactedDefault(field.Default, field.Sensitive)))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(field.Description))
		builder.WriteString(" | ")
		builder.WriteString(markdownCell(strings.Join(field.Aliases, ", ")))
		builder.WriteString(" | ")
		builder.WriteString(markdownBool(field.Sensitive))
		builder.WriteString(" |\n")
	}
	return builder.String()
}

// Markdown renders registered loader metadata as a Markdown table.
func (l *Loader) Markdown() string {
	return MarkdownVars(l.Describe())
}

func redactedDefault(value string, sensitive bool) string {
	if sensitive && value != "" {
		return RedactedValue
	}
	return value
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	if value == "" {
		return "-"
	}
	return value
}

func markdownBool(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
