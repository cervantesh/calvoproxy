package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Request compression: the ONLY place in the proxy that alters what the user
// asked for, and therefore the only one that can degrade an answer silently.
// Every choice below is subordinate to that.
//
// Off by default. Opt-in per profile. Dry-run available. Any error at all
// forwards the original body untouched. Nobody should discover their proxy
// compresses because an answer came out worse.
//
// Two engines survived the design, and the two that did not are worth recording:
//
//   - Cross-turn session dedup: DROPPED. The upstream is stateless, so not
//     resending context does not compress it — it makes the model stop seeing
//     it. That is amnesia, not compression.
//   - Semantic prose pruning: DROPPED for v1. "Semantic, deterministic, no ML"
//     is a practical contradiction: preserving code byte-for-byte while pruning
//     text means delimiting code in arbitrary Markdown, and one unclosed fence
//     turns pruning into corruption.

const (
	defaultToolCapBytes = 4096
	// toolCutMarker is deliberately explicit: a model that sees a clipped result
	// must be able to tell it was clipped, or it will reason about the gap as if
	// it were the whole answer.
	toolCutMarker = "\n… [truncado: %d bytes omitidos por CalvoProxy] …\n"
)

type compressionStat struct {
	OriginalBytes int
	SavedBytes    int
	Engines       []string
	DryRun        bool
}

func (c compressionStat) applied() bool { return c.SavedBytes > 0 && !c.DryRun }

// header renders the cmp= field: "-3.1k" style, or "off" when nothing happened.
func (c compressionStat) header() string {
	if c.SavedBytes <= 0 {
		return traceNoCompress
	}
	if c.DryRun {
		return fmt.Sprintf("dry-%s", humanBytes(c.SavedBytes))
	}
	return "-" + humanBytes(c.SavedBytes)
}

func humanBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	}
	return fmt.Sprintf("%d", n)
}

func compressEnabledFor(profile string) bool {
	raw := strings.TrimSpace(envValue("PROXY_COMPRESS_PROFILES"))
	if raw == "" {
		return false // off by default, and silence means off
	}
	for _, p := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(profile)) {
			return true
		}
	}
	return false
}

func compressDryRun() bool {
	return strings.EqualFold(strings.TrimSpace(envValue("PROXY_COMPRESS_DRYRUN")), "true")
}

func toolCapBytes() int {
	n := envInt("PROXY_COMPRESS_TOOL_LIMIT", defaultToolCapBytes)
	if n < 256 {
		n = 256 // below this the marker is most of what survives
	}
	return n
}

// safeCompress wraps compressRequest so a panic on a malformed body can never
// take down a request. Bodies come from clients and will eventually contain
// everything; a compression bug must degrade to "no compression", never to a 500.
func safeCompress(profile string, body map[string]any) (out map[string]any, stat compressionStat, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, stat, err = body, compressionStat{}, fmt.Errorf("compression panicked: %v", r)
			slog.Warn("[CalvoProxy] compression panicked; forwarding the original body",
				slog.Any("panic", r), slog.String("profile", profile))
		}
	}()
	out, stat = compressRequest(profile, body)
	return out, stat, nil
}

// compressRequest runs the enabled engines once. It returns a NEW map: the
// caller's map is the same one the fallback loop writes ["model"] into on every
// attempt, so mutating it here would corrupt the chain in ways that only surface
// on a fallback.
func compressRequest(profile string, body map[string]any) (map[string]any, compressionStat) {
	stat := compressionStat{DryRun: compressDryRun()}
	if body == nil || !compressEnabledFor(profile) {
		return body, compressionStat{}
	}

	original, ok := body["messages"].([]any)
	if !ok || len(original) == 0 {
		return body, stat
	}
	stat.OriginalBytes = approxSize(original)

	messages := cloneMessages(original)
	saved := 0
	if n := applyToolCap(messages, toolCapBytes()); n > 0 {
		saved += n
		stat.Engines = append(stat.Engines, "toolcap")
	}
	if n := applyDedup(messages); n > 0 {
		saved += n
		stat.Engines = append(stat.Engines, "dedup")
	}
	stat.SavedBytes = saved

	// Dry-run measures and walks away: it is how an operator learns what
	// compression WOULD save before trusting it with real traffic.
	if saved <= 0 || stat.DryRun {
		return body, stat
	}

	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["messages"] = messages
	return out, stat
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
// Structured content (an array of parts, as used for images) is left alone:
// there is no safe generic way to clip it, and guessing is how corruption starts.
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
// so keeping only one end picks wrong half the time.
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

// applyDedup replaces earlier copies of an identical block with a reference to
// the one that still travels.
//
// The LAST occurrence is always left whole. It is the copy the model is looking
// at now; replacing that one would take the content away exactly when it is
// needed, which is the failure this engine must never cause.
func applyDedup(messages []any) int {
	lastIndex := map[string]int{}
	for i, raw := range messages {
		content, ok := messageContent(raw)
		if !ok || len(content) < 256 {
			continue // below this, the reference costs more than the copy
		}
		lastIndex[hashContent(content)] = i
	}

	saved := 0
	seen := map[string]int{} // hash -> 1-based ordinal of the surviving copy
	for i, raw := range messages {
		content, ok := messageContent(raw)
		if !ok || len(content) < 256 {
			continue
		}
		key := hashContent(content)
		if lastIndex[key] == i {
			continue // the survivor
		}
		ordinal, known := seen[key]
		if !known {
			ordinal = lastIndex[key] + 1
			seen[key] = ordinal
		}
		replacement := fmt.Sprintf("[contenido idéntico al del mensaje #%d, omitido por CalvoProxy]", ordinal)
		if len(replacement) >= len(content) {
			continue
		}
		raw.(map[string]any)["content"] = replacement
		saved += len(content) - len(replacement)
	}
	return saved
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
