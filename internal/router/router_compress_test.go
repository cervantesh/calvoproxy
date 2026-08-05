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

// Invariant 1: with no profiles configured nothing happens at all. Off by
// default is the whole safety posture — nobody should discover their proxy
// compresses because an answer came out worse.
func TestCompress_OffByDefault(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "")
	body := bodyWith(msg("tool", strings.Repeat("x", 100_000)))
	before := mustJSON(t, body)

	out, stat := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("body changed with compression off")
	}
	if stat.applied() {
		t.Errorf("stat reports work done while off: %+v", stat)
	}
}

// Invariant 2: the input map is never mutated. execution.RequestBody["model"] is
// written per attempt on the SAME map, so mutating it here would corrupt the
// chain in ways that only show up on a fallback.
func TestCompress_NeverMutatesInput(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "100")
	body := bodyWith(msg("tool", strings.Repeat("y", 5000)))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if after := mustJSON(t, body); after != before {
		t.Errorf("input map was mutated:\n%s", after)
	}
	if mustJSON(t, out) == before {
		t.Error("nothing was compressed, so this test proves nothing")
	}
}

// Invariant 3: valid JSON tool results are left alone. Truncating JSON yields
// invalid JSON, and a corrupt result is worse than a long one.
func TestCompress_LeavesValidJSONToolResultsAlone(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "100")

	payload := map[string]any{"items": strings.Repeat("z", 5000)}
	body := bodyWith(msg("tool", mustJSON(t, payload)))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("a valid-JSON tool result was truncated; it must be left intact")
	}
}

// Invariant 4: truncation keeps both ends and says so. A tool result can carry
// its point at the start (a file) or at the end (a command's error), so keeping
// only one end picks wrong half the time.
func TestCompress_ToolCapKeepsBothEndsAndMarksTheCut(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "200")

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

// Invariant 5: only tool messages are truncated. A long user prompt is what the
// user actually asked; silently clipping it would be the worst possible bug.
func TestCompress_NeverTruncatesUserMessages(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "100")

	long := strings.Repeat("pregunta larga ", 1000)
	out, _ := compressRequest("coding", bodyWith(msg("user", long)))

	if got := firstMessageContent(t, out); got != long {
		t.Error("a user message was truncated")
	}
}

// Invariant 6: dedup replaces earlier copies and leaves the LAST one whole. The
// last copy is what the model is looking at now; replacing it with a reference
// would take the content away exactly when it is needed.
func TestCompress_DedupKeepsTheLastCopyIntact(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")

	block := strings.Repeat("contenido repetido ", 200)
	out, stat := compressRequest("coding", bodyWith(
		msg("tool", block),
		msg("assistant", "vale"),
		msg("tool", block),
		msg("assistant", "vale otra vez"),
		msg("tool", block),
	))

	contents := allMessageContents(t, out)
	if len(contents) != 5 {
		t.Fatalf("message count changed: %d", len(contents))
	}
	if contents[4] != block {
		t.Error("the last copy was replaced; the model can no longer see the content")
	}
	if contents[0] == block || contents[2] == block {
		t.Error("earlier copies were not deduplicated")
	}
	if !strings.Contains(contents[0], "idéntico") {
		t.Errorf("the replacement does not explain itself: %q", contents[0])
	}
	if stat.SavedBytes <= 0 {
		t.Errorf("SavedBytes = %d, want > 0", stat.SavedBytes)
	}
}

// Invariant 7: content that appears once is never touched by dedup.
func TestCompress_DedupIgnoresUniqueBlocks(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "1000000")

	body := bodyWith(msg("tool", "uno"), msg("tool", "dos"), msg("tool", "tres"))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Errorf("unique blocks were altered:\n%s", mustJSON(t, out))
	}
}

// Invariant 8: dry-run measures without touching. It is how an operator finds
// out what compression WOULD save before trusting it with real traffic.
func TestCompress_DryRunMeasuresWithoutApplying(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "100")
	t.Setenv("PROXY_COMPRESS_DRYRUN", "true")

	body := bodyWith(msg("tool", strings.Repeat("w", 5000)))
	before := mustJSON(t, body)

	out, stat := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("dry-run modified the body")
	}
	if stat.SavedBytes <= 0 {
		t.Errorf("dry-run reported no saving (%d); it must still measure", stat.SavedBytes)
	}
	if !stat.DryRun {
		t.Error("stat does not record that this was a dry run")
	}
}

// Invariant 9: odd shapes must not panic. Bodies come from clients and will
// contain everything eventually.
func TestCompress_SurvivesOddBodies(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "coding")

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

// A profile that is not in the opt-in list is untouched even when others are on.
func TestCompress_OnlyConfiguredProfiles(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "bulk")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "100")

	body := bodyWith(msg("tool", strings.Repeat("q", 5000)))
	before := mustJSON(t, body)

	out, _ := compressRequest("coding", body)

	if mustJSON(t, out) != before {
		t.Error("a profile outside the opt-in list was compressed")
	}
}

// Invariant 10: the saving reaches the trace, so a turn can be audited after the
// fact. cmp= is always present; here it must stop saying "off".
func TestCompress_SavingReachesTheTraceHeader(t *testing.T) {
	tr := &routeTrace{Profile: "coding"}
	tr.recordCompression(compressionStat{SavedBytes: 3100, OriginalBytes: 9000, Engines: []string{"toolcap"}})

	header := tr.header()
	if strings.Contains(header, "cmp=off") {
		t.Errorf("header still reports cmp=off after compressing: %s", header)
	}
	if !strings.Contains(header, "cmp=-3") {
		t.Errorf("header does not carry the saving: %s", header)
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

// End to end through dispatchChain: compression must actually reach the wire and
// be visible in the trace header. The unit tests above prove the engines; this
// proves they are plugged in, which is a different claim.
func TestCompress_AppliesThroughTheRouterAndShowsInTheHeader(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "simple")
	t.Setenv("PROXY_COMPRESS_TOOL_LIMIT", "300")

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
		t.Errorf("upstream received %d bytes for a %d-byte request; nothing was compressed", len(received), len(body))
	}
	if !strings.Contains(received, "truncado") {
		t.Error("the clipped tool result did not reach the upstream")
	}
	route := rec.sentHeader("X-Calvoproxy-Route")
	if strings.Contains(route, "cmp=off") {
		t.Errorf("header reports cmp=off after compressing: %s", route)
	}
}

// With compression off, the router forwards the body byte for byte. This is the
// default path, so it is the one that must never regress.
func TestCompress_RouterForwardsUntouchedWhenOff(t *testing.T) {
	t.Setenv("PROXY_COMPRESS_PROFILES", "")

	var received string
	upstream := &captureTransport{onBody: func(b string) { received = b }}
	svc := newTestService(t, &http.Client{Transport: upstream}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	huge := strings.Repeat("sin comprimir ", 500)
	rec := newHeaderSnapshotRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(http.MethodPost, "/v1/chat/completions",
		`{"messages":[{"role":"tool","content":"`+huge+`"}]}`), "k", "")

	if !strings.Contains(received, huge) {
		t.Error("the body was altered with compression off")
	}
	if route := rec.sentHeader("X-Calvoproxy-Route"); !strings.Contains(route, "cmp=off") {
		t.Errorf("header should say cmp=off: %s", route)
	}
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
