package router

import (
	"regexp"
	"strings"
)

var (
	codingPromptPattern = regexp.MustCompile(`(?i)\b(code|script|error|bug|function|función|funcion|bash|python|javascript|golang|go|html|css|codex|gemini|refactor|refactoriza|debug|depura|program|programa|codigo|código|syntax|sintaxis)\b`)
	reasonPromptPattern = regexp.MustCompile(`(?i)\b(think|solve|math|why|logic|puzzle|calculate|analyze|reason|structure|architecture|explain|razona|piensa|analiza|analizar|estructura|resuelve|matematica|matemática|logica|lógica|calcula|explicar|explica|arquitectura)\b`)
	agentPromptPattern  = regexp.MustCompile(`(?i)\b(execute|terminal|command|task|tool|system|sh|run|check|use|verify|search|find|verifica|chequea|usa|herramienta|comandos|comando|ejecuta|tarea|sistema|corre|revisa|busca|encuentra)\b`)
)

func (s *RouterService) classifyPrompt(messages []interface{}) string {
	return classifyPrompt(messages)
}

func (s *RouterService) hasImageContent(messages []interface{}) bool {
	return hasImageContent(messages)
}

func extractMessageText(content interface{}) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []interface{}:
		parts := make([]string, 0, len(typed))
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			if partType != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func contentContainsImage(content interface{}) bool {
	switch typed := content.(type) {
	case []interface{}:
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			if partType == "image_url" || partType == "input_image" || partType == "image" {
				return true
			}
			if _, ok := part["image_url"]; ok {
				return true
			}
			if _, ok := part["image"]; ok {
				return true
			}
		}
	case map[string]interface{}:
		if _, ok := typed["image_url"]; ok {
			return true
		}
		if _, ok := typed["image"]; ok {
			return true
		}
	}
	return false
}

func classifyPrompt(messages []interface{}) string {
	if len(messages) == 0 {
		return "simple"
	}
	var lastUserMsg string
	for _, m := range messages {
		msgMap, ok := m.(map[string]interface{})
		if ok && msgMap["role"] == "user" {
			lastUserMsg = strings.ToLower(extractMessageText(msgMap["content"]))
		}
	}
	if codingPromptPattern.MatchString(lastUserMsg) {
		return "coding"
	}
	if reasonPromptPattern.MatchString(lastUserMsg) {
		return "reasoning"
	}
	if agentPromptPattern.MatchString(lastUserMsg) {
		return "agent"
	}
	return "simple"
}

func hasImageContent(messages []interface{}) bool {
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]interface{})
		if !ok || msg["role"] != "user" {
			continue
		}
		if contentContainsImage(msg["content"]) {
			return true
		}
	}
	return false
}
