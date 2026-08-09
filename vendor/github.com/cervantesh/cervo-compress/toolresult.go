package compress

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ToolResultCap clips oversized tool results, keeping BOTH ends.
//
// Both ends, because a tool result carries its point at either extreme
// depending on what produced it: a file read is meaningful from the top, a
// failing command from the bottom. Keeping one end picks wrong about half the
// time, and which half is unknowable from here.
//
// This is the crudest useful engine. It knows nothing about what produced the
// output — a smarter successor would recognise command classes (test runners,
// builds, git, stack traces) and strip ANSI, progress noise and boilerplate
// while keeping failures and summaries. That engine belongs in this package
// too; this one is the floor, not the ceiling.
type ToolResultCap struct {
	// Limit is the byte budget for a single tool result. Content at or under it
	// is untouched.
	Limit int
}

func (ToolResultCap) Name() string { return "toolresult-cap" }

// cutMarker is deliberately explicit. A model shown a clipped result must be
// able to tell that it was clipped, or it will reason about the gap as though
// it were the whole answer — which is the failure mode that makes lossy
// compression dangerous rather than merely lossy.
const cutMarker = "\n… [%d bytes omitted] …\n"

// cutSignature is the invariant part of cutMarker, used to recognise content
// this engine has already clipped.
const cutSignature = " bytes omitted] …"

func (t ToolResultCap) Apply(messages []Message) ([]Message, int) {
	limit := t.Limit
	if limit < 256 {
		// Below this the marker is most of what survives, so the engine would
		// destroy content to save nothing.
		limit = 256
	}

	out := clone(messages)
	saved := 0
	// Only tool-result text, in either dialect. A user or assistant message is
	// what was actually asked.
	segments := make([]segment, 0, 4)
	for i, m := range out {
		segments = appendToolResultSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			text := seg.Text
			if len(text) <= limit {
				continue
			}
			// Truncating valid JSON produces invalid JSON, and a corrupt result is
			// worse than a long one.
			if json.Valid([]byte(text)) {
				continue
			}
			// Already clipped by this engine: re-clipping its own output would shave
			// a byte off the marker's digit count on every pass and never converge.
			// A pipeline run twice — a retry, a caller that re-compresses — must be a
			// no-op the second time.
			if strings.Contains(text, cutSignature) {
				continue
			}
			// A table written by JSONTable is clipped by whole rows, or not at all.
			//
			// The two engines are individually correct and jointly destructive:
			// JSONTable's output is deliberately NOT valid JSON, so the guard above
			// stops protecting it, and a byte clip then cuts a row in half and —
			// worse — leaves the header still claiming the original row count. A
			// 45-row table came out carrying 17 and saying 45.
			//
			// When not even one row fits the budget, nothing is done: over budget
			// and honest beats within budget and wrong, and "return the input
			// unchanged" is always a valid answer for an engine that cannot act
			// safely. Falling back to the byte clip here reopened the exact defect
			// the row clip was written to close.
			var clipped string
			var ok bool
			if isTableArtifact(text) {
				clipped, ok = clipTable(text, limit)
			} else {
				clipped, ok = clip(text, limit)
			}
			if !ok {
				continue
			}
			m = seg.replace(m, clipped)
			saved += len(text) - len(clipped)
		}
		out[i] = m
	}
	if saved == 0 {
		return messages, 0
	}
	return out, saved
}

// clip cuts text down to at most limit bytes, keeping both ends and marking the
// gap. The marker is counted against the budget so the OUTPUT fits the limit —
// otherwise a second pass would see something still over budget and clip again,
// shaving a byte off the marker's digit count each time without ever converging.
//
// The second return is false when the cut would not actually save anything.
func clip(text string, limit int) (string, bool) {
	// The marker's own length depends on the number it prints, which depends on
	// how much is cut, which depends on the marker's length. Two passes settle
	// it: size the halves with a first estimate, then re-size with the real
	// marker. The second estimate is never larger than the first, so one
	// correction is enough.
	half := (limit - len(fmt.Sprintf(cutMarker, len(text)))) / 2
	if half > 0 {
		marker := fmt.Sprintf(cutMarker, len(text)-2*half)
		half = (limit - len(marker)) / 2
	}
	if half <= 0 {
		return "", false // the marker alone would fill the budget
	}
	// Back both cut points off to a rune boundary. Slicing by byte offset can land
	// inside a multi-byte character and emit invalid UTF-8, so the model is shown
	// a broken glyph at each kept end. Dedup already takes this care with its
	// quoted prefix; the cap did not.
	headEnd := runeBoundaryBefore(text, half)
	tailStart := runeBoundaryAfter(text, len(text)-half)
	if headEnd <= 0 || tailStart >= len(text) || headEnd >= tailStart {
		return "", false
	}
	clipped := text[:headEnd] + fmt.Sprintf(cutMarker, tailStart-headEnd) + text[tailStart:]
	if len(clipped) >= len(text) {
		return "", false // the marker cost more than the cut saved
	}
	return clipped, true
}

// tableHeaderPrefix is how JSONTable introduces its output. Recognising a
// sibling engine's artifact is not layering violation here: this engine's whole
// job is deciding what may be cut, and a table has a structure that a byte
// offset does not respect.
const tableHeaderPrefix = "[table of "

// clipTable reduces a JSONTable artifact to fit the budget by dropping whole
// rows from the middle, and rewrites the header so it states what is actually
// present. The second return is false when the text is not a table, leaving the
// caller to fall back to the byte clip.
//
// Rewriting the header is the point. Clipping the rows without it produces
// something strictly worse than an oversized result: a table that declares 45
// rows and carries 17, with nothing to indicate the difference. A reader — human
// or model — has no way to know it is looking at a fragment, which is the one
// property the explicit cut marker exists to guarantee everywhere else.
func clipTable(text string, limit int) (string, bool) {
	if !strings.HasPrefix(text, tableHeaderPrefix) {
		return "", false
	}
	lines := strings.Split(text, "\n")
	// header, column names, and at least one row.
	if len(lines) < 3 {
		return "", false
	}
	columns := lines[1]
	rows := lines[2:]
	// A trailing newline leaves an empty final element that is not a row.
	if len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) == 0 {
		return "", false
	}

	// Grow the kept set one row at a time, alternating head and tail, until the
	// next row would not fit. Both ends for the same reason the byte clip keeps
	// both: the interesting record is as likely to be first as last.
	head, tail := 0, 0
	size := func(h, t, omitted int) int {
		return len(tableHeader(columns, len(rows), omitted)) + 1 + len(columns) + 1 +
			rowsSize(rows[:h]) + rowsSize(rows[len(rows)-t:])
	}
	for head+tail < len(rows) {
		nextHead, nextTail := head, tail
		if head <= tail {
			nextHead++
		} else {
			nextTail++
		}
		if size(nextHead, nextTail, len(rows)-nextHead-nextTail) > limit {
			break
		}
		head, tail = nextHead, nextTail
	}
	if head+tail == 0 {
		return "", false // not even one row fits; the byte clip is no better, but it is the caller's call
	}
	omitted := len(rows) - head - tail
	if omitted <= 0 {
		return "", false // everything fits; nothing to do
	}

	var b strings.Builder
	b.WriteString(tableHeader(columns, len(rows), omitted))
	b.WriteString("\n")
	b.WriteString(columns)
	b.WriteString("\n")
	for _, row := range rows[:head] {
		b.WriteString(row)
		b.WriteString("\n")
	}
	for _, row := range rows[len(rows)-tail:] {
		b.WriteString(row)
		b.WriteString("\n")
	}
	clipped := b.String()
	if len(clipped) >= len(text) {
		return "", false
	}
	return clipped, true
}

// tableHeader states what the table now contains and what was removed. The
// original total stays, because "17 of 45" is the useful fact; "17 rows" alone
// would be true and useless.
func tableHeader(columns string, total, omitted int) string {
	return fmt.Sprintf("[table of %d rows, %d shown, %d omitted · columns: %s]",
		total, total-omitted, omitted, strings.ReplaceAll(columns, tableSeparator, ", "))
}

func rowsSize(rows []string) int {
	n := 0
	for _, row := range rows {
		n += len(row) + 1 // the newline that follows it
	}
	return n
}

// runeBoundaryBefore returns the largest index <= i that starts a rune.
func runeBoundaryBefore(s string, i int) int {
	if i > len(s) {
		i = len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeBoundaryAfter returns the smallest index >= i that starts a rune.
func runeBoundaryAfter(s string, i int) int {
	if i < 0 {
		i = 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
