package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// `calvoproxy chat` is a diagnostic REPL, not a chat client.
//
// Trying a chain used to mean either wiring Hermes or hand-writing curl, and
// curl leaves you decoding `v1;p=coding;s=0.83;a=2;prev=...` by eye. This talks
// to the proxy exactly as an agent does — profile route, full history, SSE — and
// prints the routing decision in words after every turn.
//
// It is also the dogfooding of P1: if the trace cannot render as "served by X,
// skipped Y (breaker)", the trace is wrong, and it is far cheaper to learn that
// here than after Hermes starts parsing it.
//
// It is a CLIENT. It does not import internal/router and reimplements no part of
// the chain: everything it shows came over the wire from a running proxy.

const chatDefaultProfile = "coding"

type chatSession struct {
	baseURL   string
	profile   string
	stream    bool
	traceFull bool
	history   []map[string]any
	client    *http.Client
	out       io.Writer
}

func runChat(args []string) int {
	return runChatWith(args, os.Stdin, os.Stdout)
}

func runChatWith(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(out)
	profile := fs.String("profile", chatDefaultProfile, "profile to route through (coding, reasoning, bulk, vision, …)")
	url := fs.String("url", "", "proxy base URL (default: the configured local proxy)")
	noStream := fs.Bool("no-stream", false, "use the non-streaming response shape")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	base := strings.TrimRight(*url, "/")
	if base == "" {
		base = strings.TrimRight(proxyBaseURL(), "/")
	}

	s := &chatSession{
		baseURL: base,
		profile: *profile,
		stream:  !*noStream,
		// No overall timeout: a long-but-live stream must never be cut, which is
		// the same reason dispatchChain gives streaming requests no total budget.
		client: &http.Client{},
		out:    out,
	}

	fmt.Fprintf(out, "calvoproxy chat · %s · profile %s\n", s.baseURL, s.profile)
	fmt.Fprintln(out, "commands: /profile <n> · /reset · /trace · /quit")

	scanner := bufio.NewScanner(in)
	// Agent-sized turns exceed bufio's 64 KiB default line: a pasted file is a
	// normal thing to try here, and silently truncating it would make the tool
	// lie about what it sent.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for {
		fmt.Fprintf(out, "\n[%s] > ", s.profile)
		if !scanner.Scan() {
			// EOF (Ctrl-D) is the ordinary way to leave a REPL, not a failure.
			fmt.Fprintln(out)
			return 0
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if quit := s.command(line); quit {
				return 0
			}
			continue
		}
		s.turn(line)
	}
}

// command runs a slash command and reports whether the REPL should exit.
func (s *chatSession) command(line string) bool {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/quit", "/exit":
		return true
	case "/reset":
		s.history = nil
		fmt.Fprintln(s.out, "history cleared")
	case "/trace":
		s.traceFull = !s.traceFull
		fmt.Fprintf(s.out, "full trace: %v\n", s.traceFull)
	case "/profile":
		if len(fields) < 2 {
			fmt.Fprintln(s.out, "usage: /profile <name>")
			break
		}
		s.profile = fields[1]
		fmt.Fprintf(s.out, "profile: %s\n", s.profile)
	default:
		fmt.Fprintf(s.out, "unknown command: %s\n", fields[0])
	}
	return false
}

func (s *chatSession) endpoint() string {
	if s.profile == "" {
		return s.baseURL + "/v1/chat/completions"
	}
	return s.baseURL + "/v1/" + s.profile + "/chat/completions"
}

// turn sends one exchange. An upstream error is reported and the REPL keeps
// going: a 503 from an exhausted chain is information, and closing on it would
// hide the next turn — exactly when the operator wants to see whether the chain
// recovered.
func (s *chatSession) turn(prompt string) {
	s.history = append(s.history, map[string]any{"role": "user", "content": prompt})

	payload := map[string]any{"messages": s.history, "stream": s.stream}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(s.out, "\ncould not serialise the request: %v\n", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, s.endpoint(), bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(s.out, "\ninvalid request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// The proxy resolves the upstream key itself; a placeholder keeps clients
	// that require the header happy without ever handling a real credential.
	req.Header.Set("Authorization", "Bearer dummy")
	if s.traceFull {
		req.Header.Set("X-Calvoproxy-Trace", "full")
	}

	started := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		fmt.Fprintf(s.out, "\ncould not reach the proxy: %v\n", err)
		// The turn never happened: drop the user message so the history does not
		// accumulate prompts the model never saw.
		s.history = s.history[:len(s.history)-1]
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		fmt.Fprintf(s.out, "\n%d %s\n", resp.StatusCode, strings.TrimSpace(string(errBody)))
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			fmt.Fprintf(s.out, "retry in %ss\n", retry)
		}
		if t := renderTrace(resp.Header); t != "" {
			fmt.Fprintln(s.out, t)
		}
		s.history = s.history[:len(s.history)-1]
		return
	}

	fmt.Fprintln(s.out)
	var reply string
	if s.stream {
		reply = s.printStream(resp.Body)
	} else {
		reply = s.printWhole(resp.Body)
	}
	fmt.Fprintln(s.out)

	if t := renderTrace(resp.Header); t != "" {
		fmt.Fprintln(s.out, t)
	}
	fmt.Fprintf(s.out, "  %.1fs\n", time.Since(started).Seconds())

	if reply != "" {
		// The upstream is stateless, so the reply MUST join the history or every
		// turn silently starts a fresh conversation while looking like a chat.
		s.history = append(s.history, map[string]any{"role": "assistant", "content": reply})
	}
}

// printStream renders SSE deltas as they arrive and returns the assembled reply.
func (s *chatSession) printStream(body io.Reader) string {
	var reply strings.Builder
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content == "" {
				continue
			}
			fmt.Fprint(s.out, c.Delta.Content)
			reply.WriteString(c.Delta.Content)
		}
	}
	return reply.String()
}

func (s *chatSession) printWhole(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8*1024*1024))
	if err != nil {
		fmt.Fprintf(s.out, "truncated response: %v\n", err)
		return ""
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		fmt.Fprint(s.out, string(raw))
		return ""
	}
	content := parsed.Choices[0].Message.Content
	fmt.Fprint(s.out, content)
	return content
}

// renderTrace turns X-Calvoproxy-Route into a line a human reads. With no such
// header it returns "": inventing a decision the proxy did not report would be
// worse than saying nothing.
func renderTrace(h http.Header) string {
	route := h.Get("X-Calvoproxy-Route")
	if route == "" {
		return ""
	}
	fields := map[string]string{}
	for _, part := range strings.Split(route, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue // the leading version token
		}
		fields[key] = value
	}

	segments := []string{}
	if p := fields["p"]; p != "" {
		segments = append(segments, p)
	}
	if model := shortModel(h.Get("X-Calvoproxy-Model")); model != "" {
		segments = append(segments, model)
	}
	if s := fields["s"]; s != "" {
		segments = append(segments, "score "+s)
	}
	// Attempt 1 is the normal case. Printing "intento 1" every turn trains the
	// reader to ignore the one field that signals degradation.
	if a := fields["a"]; a != "" && a != "1" {
		if eligible := lastSegment(fields["n"]); eligible != "" {
			segments = append(segments, "attempt "+a+"/"+eligible)
		} else {
			segments = append(segments, "attempt "+a)
		}
	}
	if brk := fields["brk"]; brk != "" && brk != "0" {
		word := "excluded by breaker"
		if brk == "1" {
			word = "excluded by breaker"
		}
		segments = append(segments, brk+" "+word)
	}
	if q := fields["q"]; q != "" && q != "0" {
		segments = append(segments, q+" out of quota")
	}
	if caps := fields["caps"]; caps != "" {
		segments = append(segments, "needs "+caps)
	}
	if cmp := fields["cmp"]; cmp != "" && cmp != "off" {
		segments = append(segments, "compressed "+cmp)
	}
	if o := fields["o"]; o != "" {
		segments = append(segments, o)
	}

	var b strings.Builder
	b.WriteString("· " + strings.Join(segments, " · "))
	if prev := fields["prev"]; prev != "" {
		b.WriteString("\n  previously failed: " + renderPrev(prev))
	}
	if fields["trunc"] == "1" {
		b.WriteString("\n  (trace truncated)")
	}
	return b.String()
}

// renderPrev turns "gpt-oss-20b:429,gemma-4-31b:skip" into
// "gpt-oss-20b (429), gemma-4-31b (skip)".
func renderPrev(prev string) string {
	parts := strings.Split(prev, ",")
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		model, code, found := strings.Cut(part, ":")
		if !found {
			rendered = append(rendered, part)
			continue
		}
		rendered = append(rendered, model+" ("+code+")")
	}
	return strings.Join(rendered, ", ")
}

// shortModel drops the org prefix and the :free suffix, matching how the header
// abbreviates models. The full id stays available in X-Calvoproxy-Model.
func shortModel(model string) string {
	if model == "" {
		return ""
	}
	if _, rest, found := strings.Cut(model, "/"); found {
		model = rest
	}
	return strings.TrimSuffix(model, ":free")
}

// lastSegment returns the eligible count out of an "n=planned/caps/eligible".
func lastSegment(n string) string {
	if n == "" {
		return ""
	}
	parts := strings.Split(n, "/")
	last := parts[len(parts)-1]
	if _, err := strconv.Atoi(last); err != nil {
		return ""
	}
	return last
}
