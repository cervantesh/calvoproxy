package router

import (
	"net/http/httptest"
	"testing"
)

func TestDetermineProfile_Precedence(t *testing.T) {
	svc := NewRouterService()
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles: map[string][]string{
			"simple":    {"m1"},
			"coding":    {"m2"},
			"reasoning": {"m3"},
			"vision":    {"m4"},
		},
		Aliases: map[string]string{
			"default":   "simple",
			"simple":    "simple",
			"coding":    "coding",
			"reasoning": "reasoning",
			"vision":    "vision",
		},
	})

	tests := []struct {
		name     string
		url      string
		headers  map[string]string
		forced   string
		messages []interface{}
		expected string
	}{
		{
			name:     "forced profile when no overrides",
			url:      "http://localhost/v1/chat/completions",
			forced:   "coding",
			expected: "coding",
		},
		{
			name:     "query provider overrides forced",
			url:      "http://localhost/v1/chat/completions?provider=reasoning",
			forced:   "coding",
			expected: "reasoning",
		},
		{
			name: "header category overrides query",
			url:  "http://localhost/v1/chat/completions?provider=coding",
			headers: map[string]string{
				"X-Cervo-Category": "reasoning",
			},
			expected: "reasoning",
		},
		{
			name: "image content forces vision",
			url:  "http://localhost/v1/chat/completions?provider=coding",
			messages: []interface{}{
				map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/x.png"}},
					},
				},
			},
			expected: "vision",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.url, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			got := svc.determineProfile(req, tc.messages, tc.forced)
			if got != tc.expected {
				t.Fatalf("expected profile %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestBuildModelAttempts_RequestedModelPrependedDeduped(t *testing.T) {
	planner := NewPolicyModelAttemptPlanner(policyConfig{
		DefaultProfile: "simple",
		Profiles: map[string][]string{
			"simple": {"model-a", "model-b", "model-c"},
		},
		Aliases: map[string]string{"simple": "simple", "default": "simple"},
	})

	attempts := planner.Plan(policyDecision{Executor: providerOpenRouter}, "simple", "model-b")
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(attempts))
	}
	if attempts[0].Model != "model-b" {
		t.Fatalf("expected requested model first, got %s", attempts[0].Model)
	}
	if attempts[1].Model != "model-a" || attempts[2].Model != "model-c" {
		t.Fatalf("unexpected deduped chain order: %+v", attempts)
	}
}

func TestResolveModelAlias(t *testing.T) {
	svc := NewRouterService()
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles: map[string][]string{
			"simple": {"m1"},
			"coding": {"m2"},
		},
		Aliases: map[string]string{
			"default": "simple",
			"simple":  "simple",
			"coding":  "coding",
		},
	})

	tests := []struct {
		desc     string
		inCat    string
		inModel  string
		outCat   string
		outModel string
	}{
		{"normal model passes through", "simple", "openai/gpt-4", "simple", "openai/gpt-4"},
		{"bare capability sets category", "simple", "coding", "coding", "auto"},
		{"prefixed capability sets category", "simple", "calvoproxy/coding", "coding", "auto"},
		{"calvoproxy capability sets category", "simple", "calvoproxy/simple", "simple", "auto"},
		{"unknown prefix passes through", "simple", "anthropic/coding", "simple", "anthropic/coding"},
		{"unknown bare passes through", "simple", "unknown-model", "simple", "unknown-model"},
		{"empty model passes through", "simple", "", "simple", ""},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			gotCat, gotModel := svc.resolveModelAlias(tc.inCat, tc.inModel)
			if gotCat != tc.outCat || gotModel != tc.outModel {
				t.Fatalf("expected (%q, %q), got (%q, %q)", tc.outCat, tc.outModel, gotCat, gotModel)
			}
		})
	}
}
