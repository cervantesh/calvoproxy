package router

import (
	"net/http"
	"strings"
)

// providerAdapter owns the wire-contract differences of one upstream. Routing
// decides where to go; the adapter decides what that provider can accept.
type providerAdapter interface {
	NormalizeRequest(map[string]interface{}) map[string]interface{}
	IsCompatibilityError(status int, body string) bool
}

func adapterForProvider(provider providerID) providerAdapter {
	switch provider {
	case providerCerebras:
		return cerebrasAdapter{}
	case providerGroq:
		return groqAdapter{}
	default:
		return passthroughAdapter{}
	}
}

type passthroughAdapter struct{}

func (passthroughAdapter) NormalizeRequest(body map[string]interface{}) map[string]interface{} {
	return cloneRequestBody(body)
}

func (passthroughAdapter) IsCompatibilityError(status int, body string) bool {
	return false
}

func cloneRequestBody(body map[string]interface{}) map[string]interface{} {
	copy := make(map[string]interface{}, len(body))
	for key, value := range body {
		copy[key] = value
	}
	return copy
}

func isUnsupportedSchemaError(status int, body string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(body)
	for _, marker := range []string{
		"property '",
		"unsupported property",
		"field is not supported",
		"parameter is not supported",
		"not currently supported",
		"unsupported parameter",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
