package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// captureServer records what the REPL sent so the tests can assert on the wire
// shape rather than on the REPL's own printout: the whole point of `chat` is to
// exercise the proxy exactly as an agent would, so the request is the contract.
type captureServer struct {
	mu       sync.Mutex
	paths    []string
	bodies   []map[string]any
	headers  []http.Header
	respond  func(w http.ResponseWriter, r *http.Request, turn int)
	turnSeen int
}

func newRecordingServer(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, turn int)) (*httptest.Server, *captureServer) {
	t.Helper()
	cs := &captureServer{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cs.paths = append(cs.paths, r.URL.Path)
		cs.bodies = append(cs.bodies, body)
		cs.headers = append(cs.headers, r.Header.Clone())
		turn := cs.turnSeen
		cs.turnSeen++
		cs.mu.Unlock()
		cs.respond(w, r, turn)
	}))
	t.Cleanup(srv.Close)
	return srv, cs
}

func sseOK(text string) func(http.ResponseWriter, *http.Request, int) {
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Calvoproxy-Model", "nvidia/nemotron-3-super-120b-a12b:free")
		w.Header().Set("X-Calvoproxy-Route", "v1;p=coding;s=0.83;n=4/4/3;cmp=off")
		w.WriteHeader(http.StatusOK)
		for _, word := range strings.Fields(text) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", word+" ")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (c *captureServer) body(i int) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.bodies) {
		return nil
	}
	return c.bodies[i]
}

func (c *captureServer) path(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i >= len(c.paths) {
		return ""
	}
	return c.paths[i]
}

func messagesOf(body map[string]any) []any {
	if body == nil {
		return nil
	}
	msgs, _ := body["messages"].([]any)
	return msgs
}

// Invariant 1: the REPL posts to the chosen profile's route, carrying the whole
// history, with `stream` matching the mode. A profile route that silently fell
// back to /v1/chat/completions would test a different chain than the operator
// asked for — which is the one thing this tool exists to inspect.
func TestChat_PostsToProfileRouteWithHistory(t *testing.T) {
	srv, cs := newRecordingServer(t, sseOK("hola"))

	out := &strings.Builder{}
	code := runChatWith([]string{"--profile", "coding", "--url", srv.URL},
		strings.NewReader("que tal\n/quit\n"), out)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output:\n%s", code, out)
	}
	if got := cs.path(0); got != "/v1/coding/chat/completions" {
		t.Errorf("path = %q, want /v1/coding/chat/completions", got)
	}
	body := cs.body(0)
	if stream, _ := body["stream"].(bool); !stream {
		t.Errorf("stream = false, want true by default")
	}
	msgs := messagesOf(body)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "que tal" {
		t.Errorf("first message = %v, want user/que tal", first)
	}
}

// Invariant 2: SSE deltas are printed in order, with the transport envelope
// stripped. Leaking `data:` lines would make the tool no better than curl.
func TestChat_PrintsStreamDeltasWithoutEnvelope(t *testing.T) {
	srv, _ := newRecordingServer(t, sseOK("uno dos tres"))

	out := &strings.Builder{}
	runChatWith([]string{"--url", srv.URL}, strings.NewReader("hola\n/quit\n"), out)

	got := out.String()
	if !strings.Contains(got, "uno dos tres") {
		t.Errorf("deltas missing or out of order; output:\n%s", got)
	}
	if strings.Contains(got, "data:") || strings.Contains(got, "[DONE]") {
		t.Errorf("SSE envelope leaked into the output:\n%s", got)
	}
}

// Invariant 3: the trace header renders to something a human reads. A missing
// header must render to nothing at all — inventing a decision the proxy did not
// report would be worse than silence.
func TestChat_RenderTrace(t *testing.T) {
	tests := []struct {
		name       string
		route      string
		model      string
		wantHas    []string
		wantAbsent []string
	}{
		{
			name:       "no header invents nothing",
			route:      "",
			wantAbsent: []string{"score", "attempt", "·"},
		},
		{
			name:    "first attempt is not mentioned",
			route:   "v1;p=coding;s=0.83;n=4/4/3;cmp=off",
			model:   "nvidia/nemotron-3-super-120b-a12b:free",
			wantHas: []string{"coding", "nemotron-3-super-120b-a12b", "0.83"},
			// AttemptIndex 1 is the normal case: saying "attempt 1" every turn
			// trains the reader to ignore the one field that signals degradation.
			wantAbsent: []string{"attempt"},
		},
		{
			name:    "fallback and exclusions are explained",
			route:   "v1;p=coding;s=0.71;a=2;n=4/4/3;prev=gpt-oss-20b:429;brk=1;cmp=off",
			model:   "google/gemma-4-31b-it:free",
			wantHas: []string{"attempt 2", "gpt-oss-20b", "429", "breaker"},
		},
		{
			name:    "truncation is flagged",
			route:   "v1;p=bulk;s=0.5;n=6/6/3;cmp=off;trunc=1",
			wantHas: []string{"truncated"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.route != "" {
				h.Set("X-Calvoproxy-Route", tc.route)
			}
			if tc.model != "" {
				h.Set("X-Calvoproxy-Model", tc.model)
			}
			got := renderTrace(h)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("render missing %q; got:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("render should not contain %q; got:\n%s", absent, got)
				}
			}
		})
	}
}

// Invariant 4: an HTTP error is reported and the REPL keeps going. A 503 from an
// exhausted chain is information — closing on it would hide the next turn, which
// is exactly when the operator wants to see whether recovery happened.
func TestChat_SurvivesHTTPError(t *testing.T) {
	srv, _ := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request, turn int) {
		if turn == 0 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"All models are temporarily rate-limited"}}`)
			return
		}
		sseOK("recuperado")(w, r, turn)
	})

	out := &strings.Builder{}
	code := runChatWith([]string{"--url", srv.URL},
		strings.NewReader("uno\ndos\n/quit\n"), out)

	got := out.String()
	if code != 0 {
		t.Errorf("exit code = %d, want 0: an upstream error is not a REPL failure", code)
	}
	if !strings.Contains(got, "503") {
		t.Errorf("status not surfaced; output:\n%s", got)
	}
	if !strings.Contains(got, "recuperado") {
		t.Errorf("REPL did not survive to serve the next turn; output:\n%s", got)
	}
}

// Invariant 5: the assistant reply joins the history, so turn two carries three
// messages. The upstream is stateless — dropping the reply would make every turn
// a fresh conversation while looking like a working chat.
func TestChat_AppendsAssistantReplyToHistory(t *testing.T) {
	srv, cs := newRecordingServer(t, sseOK("vale"))

	out := &strings.Builder{}
	runChatWith([]string{"--url", srv.URL}, strings.NewReader("uno\ndos\n/quit\n"), out)

	msgs := messagesOf(cs.body(1))
	if len(msgs) != 3 {
		t.Fatalf("second turn carried %d messages, want 3 (user, assistant, user)", len(msgs))
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "assistant" {
		t.Errorf("message[1].role = %v, want assistant", second["role"])
	}
	if !strings.Contains(fmt.Sprint(second["content"]), "vale") {
		t.Errorf("assistant content = %v, want the streamed reply", second["content"])
	}
}

// Invariant 6: slash commands do their job. /profile must change the route of
// the NEXT turn, /reset must drop the history, and /quit must exit cleanly.
func TestChat_SlashCommands(t *testing.T) {
	srv, cs := newRecordingServer(t, sseOK("ok"))

	out := &strings.Builder{}
	code := runChatWith([]string{"--profile", "coding", "--url", srv.URL},
		strings.NewReader("uno\n/profile bulk\ndos\n/reset\ntres\n/quit\n"), out)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := cs.path(0); got != "/v1/coding/chat/completions" {
		t.Errorf("turn 1 path = %q, want the starting profile", got)
	}
	if got := cs.path(1); got != "/v1/bulk/chat/completions" {
		t.Errorf("turn 2 path = %q, want /profile to have switched it", got)
	}
	if msgs := messagesOf(cs.body(2)); len(msgs) != 1 {
		t.Errorf("turn 3 carried %d messages, want 1 after /reset", len(msgs))
	}
}

// Invariant 6b: EOF is /quit. A REPL that treats Ctrl-D as an error exits
// non-zero on the most ordinary way to leave it.
func TestChat_EOFExitsCleanly(t *testing.T) {
	srv, _ := newRecordingServer(t, sseOK("ok"))

	out := &strings.Builder{}
	if code := runChatWith([]string{"--url", srv.URL}, strings.NewReader("hola\n"), out); code != 0 {
		t.Errorf("exit code = %d on EOF, want 0", code)
	}
}

// Invariant 7: --no-stream posts stream:false and reads the non-streaming shape.
func TestChat_NoStreamReadsMessageContent(t *testing.T) {
	srv, cs := newRecordingServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Calvoproxy-Route", "v1;p=coding;s=0.9;n=4/4/3;cmp=off")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"respuesta entera"}}]}`)
	})

	out := &strings.Builder{}
	runChatWith([]string{"--url", srv.URL, "--no-stream"},
		strings.NewReader("hola\n/quit\n"), out)

	if stream, _ := cs.body(0)["stream"].(bool); stream {
		t.Errorf("stream = true, want false with --no-stream")
	}
	if !strings.Contains(out.String(), "respuesta entera") {
		t.Errorf("non-streaming content not printed; output:\n%s", out)
	}
}

// /trace toggles the opt-in header, which is the only way a client can ask for
// the full decision without the admin gate.
func TestChat_TraceCommandSendsOptInHeader(t *testing.T) {
	srv, cs := newRecordingServer(t, sseOK("ok"))

	out := &strings.Builder{}
	runChatWith([]string{"--url", srv.URL}, strings.NewReader("uno\n/trace\ndos\n/quit\n"), out)

	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.headers) < 2 {
		t.Fatalf("expected 2 turns, got %d", len(cs.headers))
	}
	if got := cs.headers[0].Get("X-Calvoproxy-Trace"); got != "" {
		t.Errorf("turn 1 sent X-Calvoproxy-Trace=%q, want unset by default", got)
	}
	if got := cs.headers[1].Get("X-Calvoproxy-Trace"); got != "full" {
		t.Errorf("turn 2 X-Calvoproxy-Trace = %q, want full after /trace", got)
	}
}
