package compress

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Dedup replaces repeated copies of an identical block inside ONE history with
// a reference to the copy that still travels.
//
// This is lossless in the only sense that matters: nothing leaves the request.
// The content the reference points at is still in the same payload, further
// down. That is what separates this from dropping history, which against a
// stateless upstream does not compress anything — it makes the model stop
// seeing it.
//
// The LAST occurrence is always left whole. It is the copy the model is looking
// at now; replacing that one would take the content away exactly when it is
// needed.
//
// The reference quotes the content it points at, rather than naming a message
// ordinal.
//
// It says "the later block" because the LAST copy is the one left whole, so the
// intact payload is always further down the history than the reference to it.
// The first wording said "earlier" and was therefore wrong in every case — the
// marker pointed the reader away from the only surviving copy. The ordinal form — "[identical to message #5]" — was addressed to a
// reader that does not exist: the model receives an unnumbered list and cannot
// count to message five, so the marker degenerated into "you have seen this
// before" while sounding precise. A short quoted prefix of the target is
// something the model can actually scan the conversation for, and it is honest
// about being a hint rather than an address.
//
// It stays a hint. Two different blocks that begin with the same line produce
// the same reference; resolving that would need an identifier the model can
// look up, which is retrieval, not compression. A caller that can inject its
// own retrieval affordance — a tool the model may call to fetch a hash back —
// should still prefer that, and use MinBytes to keep this engine off the blocks
// it handles itself.
type Dedup struct {
	// MinBytes is the size below which a block is left alone: under it the
	// reference costs more than the copy.
	MinBytes int
}

func (Dedup) Name() string { return "dedup" }

// dedupSignature is the invariant part of the reference, used to recognise
// content this engine has already replaced. Deduplicating a reference would
// produce a reference to a reference — shorter than the original block, so the
// size guard would not catch it, and the second pass over a history would not
// be the no-op it must be.
const dedupSignature = "[identical to the later block starting "

// dedupPrefixRunes bounds the quoted prefix. Long enough to be locatable in a
// page of output, short enough that the reference stays much smaller than what
// it replaces. Counted in runes, not bytes, so a truncation never splits a
// character.
const dedupPrefixRunes = 48

// segID identifies one segment in the history. Content that repeats inside a
// single message is as deduplicable as content that repeats across messages, so
// the survivor has to be named by both coordinates.
type segID struct {
	message int
	segment int
}

func (d Dedup) Apply(messages []Message) ([]Message, int) {
	min := d.MinBytes
	if min < 64 {
		min = 64
	}

	// First pass: where does each distinct block appear for the LAST time?
	lastAt := map[string]segID{}
	segments := make([]segment, 0, 4)
	for i, m := range messages {
		segments = appendContentSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			if !dedupCandidate(seg.Text, min) {
				continue
			}
			lastAt[hash(seg.Text)] = segID{message: i, segment: seg.index}
		}
	}

	out := clone(messages)
	saved := 0
	for i, m := range out {
		segments = appendContentSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			if !dedupCandidate(seg.Text, min) {
				continue
			}
			if lastAt[hash(seg.Text)] == (segID{message: i, segment: seg.index}) {
				continue // the survivor
			}
			ref, ok := dedupReference(seg.Text)
			if !ok || len(ref) >= len(seg.Text) {
				continue
			}
			m = seg.replace(m, ref)
			saved += len(seg.Text) - len(ref)
		}
		out[i] = m
	}
	if saved == 0 {
		return messages, 0
	}
	return out, saved
}

// dedupCandidate is what this engine is willing to consider at all. It must
// give the same answer in both passes, or the survivor bookkeeping would point
// at a segment the second pass skipped.
func dedupCandidate(text string, min int) bool {
	return len(text) >= min && !strings.Contains(text, dedupSignature)
}

// dedupReference builds the marker that replaces a repeated block. The second
// return is false when no usable prefix could be quoted — content that is only
// whitespace and control characters has nothing a model could search for, and
// pointing at it with an empty quote would be worse than leaving the copy.
func dedupReference(text string) (string, bool) {
	prefix, ok := quotedPrefix(text)
	if !ok {
		return "", false
	}
	return dedupSignature + `"` + prefix + `"]`, true
}

// quotedPrefix renders the head of a block as something safe to put inside
// quotes on one line: the first line with real content, stripped of quotes and
// control characters, bounded in length.
//
// Sanitising rather than escaping is deliberate. The marker is read by a model,
// not parsed, so a backslash-escaped quote would only add a character to
// mis-read; and a marker that could contain a newline would break the one thing
// callers can rely on about it — that it is a single short line.
func quotedPrefix(text string) (string, bool) {
	line := ""
	for _, candidate := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			line = trimmed
			break
		}
	}
	if line == "" {
		return "", false
	}

	var b strings.Builder
	count := 0
	truncated := false
	for _, r := range line {
		if count == dedupPrefixRunes {
			truncated = true
			break
		}
		switch {
		case r == '"' || r == '\\':
			continue // would need escaping; the character carries no meaning here
		case r == utf8.RuneError:
			continue // invalid UTF-8: quoting it would emit a replacement glyph
		case unicode.IsControl(r):
			continue
		}
		b.WriteRune(r)
		count++
	}
	if b.Len() == 0 {
		return "", false
	}
	if truncated {
		b.WriteString("…")
	}
	return b.String(), true
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
