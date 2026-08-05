package router

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// A transport guard, not context management.
//
// This file used to hold two compression engines. It no longer does, and the
// reason is worth keeping: deciding what may be dropped from a conversation
// requires knowing that conversation — which tool result still matters, what
// the user is doing, whether a block can be fetched back if the model asks for
// it. The proxy sees a stateless snapshot and knows none of that. Hermes and
// the coding agents do; they own the conversation. Those engines now live in
// github.com/cervantesh/cervo-compress, where the client that should be making
// the call can import them.
//
// What DOES belong here is a size guard, in the same family as
// PROXY_MAX_RESPONSE_BYTES: a single tool result of several hundred kilobytes
// is a transport problem before it is a context problem. It gets clipped, both
// ends kept, and the client is told. That is a statement about what this proxy
// will carry — not a judgement about what the model needs.
//
// Off unless PROXY_TOOL_RESULT_LIMIT is set. The default is to forward exactly
// what the caller sent.

const (
	// toolCutMarker is deliberately explicit. A model shown a clipped result
	// must be able to tell that it was clipped, or it will reason about the gap
	// as though it were the whole answer.
	toolCutMarker = "\n… [truncado: %d bytes omitidos por CalvoProxy] …\n"
	// minToolResultLimit is the floor. Below it the marker is most of what
	// survives, so the guard would destroy content to save nothing.
	minToolResultLimit = 512
)

type compressionStat struct {
	OriginalBytes int
	SavedBytes    int
	Engines       []string
}

func (c compressionStat) applied() bool { return c.SavedBytes > 0 }

// header renders the cmp= field: "-3.1k" style, or "off" when nothing happened.
func (c compressionStat) header() string {
	if c.SavedBytes <= 0 {
		return traceNoCompress
	}
	return "-" + humanBytes(c.SavedBytes)
}

func humanBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	}
	return fmt.Sprintf("%d", n)
}

// toolResultLimit is the per-tool-result byte budget, or 0 when the guard is
// off. Off is the default: the proxy forwards what it was given.
func toolResultLimit() int {
	raw := strings.TrimSpace(envValue("PROXY_TOOL_RESULT_LIMIT"))
	if raw == "" {
		return 0
	}
	n := envInt("PROXY_TOOL_RESULT_LIMIT", 0)
	if n <= 0 {
		return 0
	}
	if n < minToolResultLimit {
		return minToolResultLimit
	}
	return n
}

// safeCompress wraps the guard so a malformed body can never take down a
// request. Bodies come from clients and will eventually contain everything; a
// bug here must degrade to "forwarded as-is", never to a 500.
func safeCompress(profile string, body map[string]any) (out map[string]any, stat compressionStat, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, stat, err = body, compressionStat{}, fmt.Errorf("tool-result guard panicked: %v", r)
			slog.Warn("[CalvoProxy] tool-result guard panicked; forwarding the original body",
				slog.Any("panic", r), slog.String("profile", profile))
		}
	}()
	out, stat = compressRequest(profile, body)
	return out, stat, nil
}

// compressRequest applies the transport guard. It returns a NEW map: the
// caller's map is the same one the fallback loop writes ["model"] into on every
// attempt, so mutating it here would corrupt the chain in ways that only
// surface on a fallback.
func compressRequest(_ string, body map[string]any) (map[string]any, compressionStat) {
	limit := toolResultLimit()
	if body == nil || limit <= 0 {
		return body, compressionStat{}
	}

	original, ok := body["messages"].([]any)
	if !ok || len(original) == 0 {
		return body, compressionStat{}
	}

	messages := cloneMessages(original)
	saved := applyToolCap(messages, limit)
	if saved <= 0 {
		return body, compressionStat{}
	}

	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["messages"] = messages
	return out, compressionStat{
		OriginalBytes: approxSize(original),
		SavedBytes:    saved,
		Engines:       []string{"toolcap"},
	}
}

// cloneMessages copies each message map so edits never reach the caller's.
func cloneMessages(original []any) []any {
	out := make([]any, len(original))
	for i, raw := range original {
		entry, ok := raw.(map[string]any)
		if !ok {
			out[i] = raw
			continue
		}
		copied := make(map[string]any, len(entry))
		for k, v := range entry {
			copied[k] = v
		}
		out[i] = copied
	}
	return out
}

func approxSize(messages []any) int {
	total := 0
	for _, raw := range messages {
		if content, ok := messageContent(raw); ok {
			total += len(content)
		}
	}
	return total
}

// messageContent returns a message's content only when it is a plain string.
// Structured content (an array of parts, as used for images and for Anthropic
// tool results) is left alone: there is no safe generic way to clip it, and
// guessing at a shape this code does not understand is how corruption starts.
func messageContent(raw any) (string, bool) {
	entry, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	content, ok := entry["content"].(string)
	return content, ok
}

func messageRole(raw any) string {
	entry, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := entry["role"].(string)
	return role
}

// applyToolCap clips oversized tool results, keeping BOTH ends. A tool result
// can carry its point at the start (a file) or at the end (a command's error),
// so keeping only one end picks wrong about half the time.
//
// A result that parses as JSON is never touched: truncating JSON yields invalid
// JSON, and a corrupt result is worse than a long one.
func applyToolCap(messages []any, limit int) int {
	saved := 0
	for _, raw := range messages {
		if messageRole(raw) != "tool" {
			continue // never a user or assistant message: that is what was asked
		}
		content, ok := messageContent(raw)
		if !ok || len(content) <= limit {
			continue
		}
		if json.Valid([]byte(content)) {
			continue
		}
		half := limit / 2
		omitted := len(content) - 2*half
		clipped := content[:half] + fmt.Sprintf(toolCutMarker, omitted) + content[len(content)-half:]
		if len(clipped) >= len(content) {
			continue // the marker cost more than the cut saved
		}
		raw.(map[string]any)["content"] = clipped
		saved += len(content) - len(clipped)
	}
	return saved
}
