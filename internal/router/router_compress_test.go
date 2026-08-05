package router

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func msg(role, content string) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func bodyWith(messages ...map[string]any) map[string]any {
	raw := make([]any, 0, len(messages))
	for _, m := range messages {
		raw = append(raw, m)
	}
	return map[string]any{"model": "org/x:free", "messages": raw}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// Off by default: without PROXY_TOOL_RESULT_LIMIT the proxy forwards exactly
// what the caller sent. Deciding what a conversation may lose is not the
// proxy's call, so the default has to be "carry it".
func TestToolGuard_OffByDefault(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "")
	body := bodyWith(msg("tool", strings.Repeat("x", 100_000)))
	before := mustJSON(t, body)

	out, stat := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("body changed with the guard off")
	}
	if stat.applied() {
		t.Errorf("stat reports work done while off: %+v", stat)
	}
}

// The input map is never mutated. execution.RequestBody["model"] is written per
// attempt on the SAME map, so mutating it here would corrupt the chain in ways
// that only show up on a fallback.
func TestToolGuard_NeverMutatesInput(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")
	body := bodyWith(msg("tool", strings.Repeat("y", 5000)))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if after := mustJSON(t, body); after != before {
		t.Errorf("input map was mutated:\n%s", after)
	}
	if mustJSON(t, out) == before {
		t.Error("nothing was clipped, so this test proves nothing")
	}
}

// Valid JSON tool results are left alone: truncating JSON yields invalid JSON,
// and a corrupt result is worse than a long one.
func TestToolGuard_LeavesValidJSONAlone(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")

	payload := map[string]any{"items": strings.Repeat("z", 5000)}
	body := bodyWith(msg("tool", mustJSON(t, payload)))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("a valid-JSON tool result was truncated; it must be left intact")
	}
}

// Truncation keeps both ends and says so. A tool result can carry its point at
// the start (a file) or at the end (a command's error).
func TestToolGuard_KeepsBothEndsAndMarksTheCut(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")

	content := "INICIO-IMPORTANTE" + strings.Repeat("relleno ", 500) + "FINAL-IMPORTANTE"
	out, stat := compressRequest("coding", bodyWith(msg("tool", content)))

	got := firstMessageContent(t, out)
	if !strings.Contains(got, "INICIO-IMPORTANTE") {
		t.Error("the head was dropped")
	}
	if !strings.Contains(got, "FINAL-IMPORTANTE") {
		t.Error("the tail was dropped")
	}
	if !strings.Contains(got, "truncado") {
		t.Errorf("the cut is not marked, so the model cannot tell: %s", got)
	}
	if stat.SavedBytes <= 0 {
		t.Errorf("SavedBytes = %d, want > 0", stat.SavedBytes)
	}
}

// Only tool messages are clipped. A long user prompt is what the user actually
// asked; silently trimming it would be the worst possible bug.
func TestToolGuard_NeverTouchesUserMessages(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")

	long := strings.Repeat("pregunta larga ", 1000)
	out, _ := compressRequest("coding", bodyWith(msg("user", long)))

	if got := firstMessageContent(t, out); got != long {
		t.Error("a user message was clipped")
	}
}

// An absurdly small limit is clamped to the floor rather than destroying
// content to save nothing.
func TestToolGuard_ClampsAbsurdLimits(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "1")

	out, saved := compressRequest("coding", bodyWith(msg("tool", strings.Repeat("q", 9000))))

	if saved.SavedBytes <= 0 {
		t.Fatal("nothing was clipped")
	}
	if got := len(firstMessageContent(t, out)); got < minToolResultLimit {
		t.Errorf("clipped to %d bytes, below the %d floor", got, minToolResultLimit)
	}
}

// Odd shapes must not panic. Bodies come from clients and will contain
// everything eventually.
func TestToolGuard_SurvivesOddBodies(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")

	cases := []map[string]any{
		{},
		{"messages": nil},
		{"messages": "no es una lista"},
		{"messages": []any{nil, 42, "texto", map[string]any{"role": 1}}},
		{"messages": []any{map[string]any{"role": "tool"}}},
		{"messages": []any{map[string]any{"role": "tool", "content": []any{"partes"}}}},
	}
	for i, body := range cases {
		if _, _, err := safeCompress("coding", body); err != nil {
			t.Errorf("case %d panicked or errored: %v", i, err)
		}
	}
}

// The saving reaches the trace, so a turn can be audited after the fact. cmp= is
// always present; here it must stop saying "off".
func TestToolGuard_SavingReachesTheTraceHeader(t *testing.T) {
	tr := &routeTrace{Profile: "coding"}
	tr.recordCompression(compressionStat{SavedBytes: 3100, OriginalBytes: 9000, Engines: []string{"toolcap"}})

	header := tr.header()
	if strings.Contains(header, "cmp=off") {
		t.Errorf("header still reports cmp=off after clipping: %s", header)
	}
	if !strings.Contains(header, "cmp=-3") {
		t.Errorf("header does not carry the saving: %s", header)
	}
}

// End to end through dispatchChain: the guard must actually reach the wire and
// be visible in the trace. Proving the function works and proving it is plugged
// in are different claims.
func TestToolGuard_AppliesThroughTheRouterAndShowsInTheHeader(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "600")

	var received string
	upstream := &captureTransport{onBody: func(b string) { received = b }}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	huge := strings.Repeat("resultado de herramienta muy largo ", 500)
	body := `{"messages":[{"role":"tool","content":"` + huge + `"},{"role":"user","content":"resume"}]}`

	rec := newHeaderSnapshotRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions", body), "k", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(received) >= len(body) {
		t.Errorf("upstream received %d bytes for a %d-byte request; nothing was clipped", len(received), len(body))
	}
	if !strings.Contains(received, "truncado") {
		t.Error("the clipped tool result did not reach the upstream")
	}
	if route := rec.sentHeader("X-Calvoproxy-Route"); strings.Contains(route, "cmp=off") {
		t.Errorf("header reports cmp=off after clipping: %s", route)
	}
}

// With the guard off the router forwards the body byte for byte. This is the
// default path, so it is the one that must never regress.
func TestToolGuard_RouterForwardsUntouchedWhenOff(t *testing.T) {
	t.Setenv("PROXY_TOOL_RESULT_LIMIT", "")

	var received string
	upstream := &captureTransport{onBody: func(b string) { received = b }}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	huge := strings.Repeat("sin recortar ", 500)
	rec := newHeaderSnapshotRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
		`{"messages":[{"role":"tool","content":"`+huge+`"}]}`), "k", "")

	if !strings.Contains(received, huge) {
		t.Error("the body was altered with the guard off")
	}
	if route := rec.sentHeader("X-Calvoproxy-Route"); !strings.Contains(route, "cmp=off") {
		t.Errorf("header should say cmp=off: %s", route)
	}
}

func firstMessageContent(t *testing.T, body map[string]any) string {
	t.Helper()
	return allMessageContents(t, body)[0]
}

func allMessageContents(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		entry, _ := m.(map[string]any)
		content, _ := entry["content"].(string)
		out = append(out, content)
	}
	return out
}

// captureTransport records the request body and returns a minimal 200.
type captureTransport struct{ onBody func(string) }

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		c.onBody(string(raw))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
	}, nil
}
