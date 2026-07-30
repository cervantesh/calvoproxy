package cervohttpkit

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type ErrorBody struct {
	Error string `json:"error"`
}

type HealthBody struct {
	Service   string    `json:"service"`
	Status    string    `json:"status"`
	Ready     bool      `json:"ready"`
	Timestamp time.Time `json:"timestamp"`
}

func JSON(w http.ResponseWriter, statusCode int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(value)
}

func JSONError(w http.ResponseWriter, statusCode int, message string) {
	JSON(w, statusCode, ErrorBody{Error: message})
}

func RequestID(r *http.Request, headerNames ...string) string {
	for _, name := range headerNames {
		if id := strings.TrimSpace(r.Header.Get(name)); id != "" {
			return id
		}
	}
	return ""
}

func FirstHeader(r *http.Request, headerNames ...string) string {
	for _, name := range headerNames {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func HealthHandler(service string, ready func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isReady := true
		if ready != nil {
			isReady = ready()
		}
		status := "ok"
		code := http.StatusOK
		if !isReady {
			status = "unavailable"
			code = http.StatusServiceUnavailable
		}
		JSON(w, code, HealthBody{Service: service, Status: status, Ready: isReady, Timestamp: time.Now().UTC()})
	}
}

func ReadyHandler(ready func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isReady := ready == nil || ready()
		if !isReady {
			JSONError(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		JSON(w, http.StatusOK, map[string]bool{"ready": true})
	}
}
