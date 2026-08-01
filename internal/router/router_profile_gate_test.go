package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The whole point of a fail-closed profile is that naming it means something.
// Before this gate an unknown name was silently ignored and the profile came
// from keyword-classifying the prompt, so `critic` returned 200 from whatever
// chain classification picked — the caller believing it got the critic.
func TestRejectUnknownProfileAnswers404(t *testing.T) {
	s := &RouterService{policy: policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"a/b:free"}, "critic": {"c/d:free"}},
		Aliases:        map[string]string{"review": "critic"},
	}}

	for _, name := range []string{"critc", "planning", "noexiste"} {
		rec := httptest.NewRecorder()
		if !s.rejectUnknownProfile(rec, name) {
			t.Errorf("%q names no profile and must be rejected", name)
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q: got HTTP %d, want 404", name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "coding") {
			t.Errorf("%q: the error should list the known profiles, got %q", name, rec.Body.String())
		}
	}
	if got := s.counters.unknownProfileRejected.Load(); got != 3 {
		t.Errorf("rejections counted = %d, want 3", got)
	}
}

func TestRejectUnknownProfileAllowsRealNames(t *testing.T) {
	s := &RouterService{policy: policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"a/b:free"}, "critic": {"c/d:free"}},
		Aliases:        map[string]string{"review": "critic"},
	}}

	// Profiles, aliases, an empty model (classification decides) and — crucially —
	// any vendor/model pin, including one absent from every chain. Rejecting those
	// would break pinning, which is a supported feature.
	// Profile names are normalized (case and surrounding space), so "Critic "
	// resolves and must be accepted rather than 404'd on a cosmetic difference.
	for _, name := range []string{"", "auto", "AUTO", "coding", "critic", "review", "Critic ", "a/b:free", "some/other-model:free"} {
		rec := httptest.NewRecorder()
		if s.rejectUnknownProfile(rec, name) {
			t.Errorf("%q must be accepted, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// Images on a system or assistant turn used to bypass the vision gate, which is
// fail-closed: undetected images route to a chain with no vision model.
func TestHasImageContentScansEveryRole(t *testing.T) {
	imagePart := []interface{}{map[string]interface{}{
		"type":      "image_url",
		"image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"},
	}}
	for _, role := range []string{"user", "system", "assistant", "tool"} {
		msgs := []interface{}{map[string]interface{}{"role": role, "content": imagePart}}
		if !hasImageContent(msgs) {
			t.Errorf("an image on role %q was not detected", role)
		}
	}
	// Anthropic's block shape, which uses type:"image" rather than image_url.
	anthropic := []interface{}{map[string]interface{}{
		"role":    "system",
		"content": []interface{}{map[string]interface{}{"type": "image", "source": map[string]interface{}{}}},
	}}
	if !hasImageContent(anthropic) {
		t.Error("Anthropic image block on a system turn was not detected")
	}
	none := []interface{}{map[string]interface{}{"role": "user", "content": "just text"}}
	if hasImageContent(none) {
		t.Error("text-only content must not read as an image")
	}
}

// An empty or malformed tools field asks for nothing, but used to force the
// tools capability requirement and shrink the eligible chain for no reason.
func TestHasRequestToolsRequiresNonEmptyArray(t *testing.T) {
	truthy := []map[string]interface{}{
		{"tools": []interface{}{map[string]interface{}{"type": "function"}}},
		{"functions": []interface{}{map[string]interface{}{"name": "f"}}},
	}
	for _, body := range truthy {
		if !hasRequestTools(body) {
			t.Errorf("a non-empty array must require tools: %v", body)
		}
	}
	falsy := []map[string]interface{}{
		{},
		{"tools": nil},
		{"tools": []interface{}{}},
		{"tools": map[string]interface{}{}},
		{"tools": "not-an-array"},
		{"functions": []interface{}{}},
		{"functions": map[string]interface{}{}},
	}
	for _, body := range falsy {
		if hasRequestTools(body) {
			t.Errorf("this asks for no tools and must not require them: %v", body)
		}
	}
}

// PROXY_<X> must reach the vendored library, which only reads CERVO_<X>.
// Bridging rather than renaming is what keeps the two halves from disagreeing.
func TestBridgeLegacyPolicyEnv(t *testing.T) {
	t.Setenv("CERVO_MODEL_DEFAULT_PROFILE", "")
	t.Setenv("PROXY_MODEL_DEFAULT_PROFILE", "critic")
	bridgeLegacyPolicyEnv()
	if got := os.Getenv("CERVO_MODEL_DEFAULT_PROFILE"); got != "critic" {
		t.Errorf("PROXY_ name did not reach the legacy name: got %q", got)
	}

	// The legacy name wins when both are set, so upgrading cannot silently
	// change the behavior of a deployment that already sets the old one.
	t.Setenv("CERVO_MODEL_POLICY_MODE", "strict-legacy")
	t.Setenv("PROXY_MODEL_POLICY_MODE", "something-else")
	bridgeLegacyPolicyEnv()
	if got := os.Getenv("CERVO_MODEL_POLICY_MODE"); got != "strict-legacy" {
		t.Errorf("legacy value was overwritten: got %q", got)
	}
}

// /v1/embeddings bills real credit — OpenRouter has no free embedding model —
// and is the one endpoint with no chain, breaker or fallback behind it.
func TestEmbeddingsRefusedUnlessOptedIn(t *testing.T) {
	s := &RouterService{policy: policyConfig{DefaultProfile: "coding"}}

	t.Setenv("PROXY_ALLOW_PAID_EMBEDDINGS", "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"x"}`))
	s.RouteRequestWithProvider(rec, req, "k", "")

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("got HTTP %d, want 402", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(rec.Body.String(), "PROXY_ALLOW_PAID_EMBEDDINGS") {
		t.Errorf("the refusal must name the opt-in, got %q", rec.Body.String())
	}
	if got := s.counters.paidEmbeddingRefused.Load(); got != 1 {
		t.Errorf("refusal counted = %d, want 1", got)
	}
}

// An agent sends tools on essentially every turn, so the old "skip the vision
// chain whenever tools are required" guard meant it never took that path: every
// image it saw was answered by whatever the capability rescue happened to find,
// which in practice was the weakest vision-capable model.
func TestImageRequestUsesVisionChainEvenWithTools(t *testing.T) {
	s := &RouterService{
		policy: policyConfig{
			DefaultProfile: "coding",
			Profiles: map[string][]string{
				"coding": {"text/only:free"},
				"vision": {"sees/and-calls:free"},
			},
		},
		capabilities: newCapabilityIndex(map[string][]string{
			"text/only:free":      {"tools"},
			"sees/and-calls:free": {"vision", "tools"},
		}),
	}
	messages := []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{map[string]interface{}{
			"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AA"},
		}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))

	if got := s.determineProfile(req, messages, "", true); got != "vision" {
		t.Errorf("an image request with tools should use the curated vision chain, got %q", got)
	}
	if got := s.determineProfile(req, messages, "", false); got != "vision" {
		t.Errorf("an image request without tools should use the vision chain, got %q", got)
	}
}

// Fail-closed: if the vision chain cannot serve the request, do NOT switch to
// it — stay put and let the capability filter rescue, rather than routing into
// a chain guaranteed to fail.
func TestImageRequestKeepsProfileWhenVisionChainCannotServe(t *testing.T) {
	s := &RouterService{
		policy: policyConfig{
			DefaultProfile: "coding",
			Profiles: map[string][]string{
				"coding": {"text/only:free"},
				"vision": {"sees/no-tools:free"}, // vision, but no tool calling
			},
		},
		capabilities: newCapabilityIndex(map[string][]string{
			"text/only:free":     {"tools"},
			"sees/no-tools:free": {"vision"},
		}),
	}
	messages := []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{map[string]interface{}{
			"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AA"},
		}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))

	if got := s.determineProfile(req, messages, "", true); got == "vision" {
		t.Error("must not switch into a vision chain that cannot satisfy the tools requirement")
	}
	// Without tools that same chain serves fine.
	if got := s.determineProfile(req, messages, "", false); got != "vision" {
		t.Errorf("without tools the vision chain serves; got %q", got)
	}
}

// The vision chain is curated. An index with no data for a model — auto-derive
// has not run, or the model is new — must not override that configuration and
// strand image requests on a text-only chain.
func TestImageRequestTrustsCuratedChainForUnknownModels(t *testing.T) {
	s := &RouterService{
		policy: policyConfig{
			DefaultProfile: "coding",
			Profiles: map[string][]string{
				"coding": {"text/only:free"},
				"vision": {"brand/new-model:free"}, // nothing known about it
			},
		},
		capabilities: newCapabilityIndex(map[string][]string{
			"text/only:free": {"tools"},
		}),
	}
	messages := []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{map[string]interface{}{
			"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AA"},
		}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))

	if got := s.determineProfile(req, messages, "", true); got != "vision" {
		t.Errorf("an unknown model in the curated chain must not veto the switch, got %q", got)
	}
}
