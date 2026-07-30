package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyHTTPClassifierClassifiesKnownPaths(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(headerRequestID, "req-1")
	req.Header.Set(headerUser, "cervantes")

	facts := proxyHTTPClassifier.FactsFromHTTPRequest(req)

	if facts.ID != "req-1" || facts.User != "cervantes" {
		t.Fatalf("unexpected request facts: %+v", facts)
	}
	if facts.OperationHint != capChatCompletion {
		t.Fatalf("expected chat operation, got %q", facts.OperationHint)
	}
}

func TestRequestFromFactsCarriesRiskAsMetadata(t *testing.T) {
	req := requestFromFacts(requestFacts{
		ID:            "req-1",
		User:          "cervantes",
		Risk:          "sensitive",
		OperationHint: capSecretLookup,
		Metadata:      map[string]string{"profile": "admin"},
	})

	if req.Metadata["risk"] != "sensitive" || req.Metadata["profile"] != "admin" {
		t.Fatalf("expected v3 metadata to include risk and profile, got %+v", req.Metadata)
	}
}
