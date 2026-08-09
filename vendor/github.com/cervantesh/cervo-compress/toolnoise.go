package compress

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolNoise strips the parts of command output that carry no information: ANSI
// escapes, progress redraws, and runs of identical lines.
//
// This is the engine that matters most for agent workloads. A test run, a build
// or an install is mostly ceremony — spinners, percentages, a thousand
// "downloading" lines — wrapped around a handful of lines someone actually needs
// to read. Clipping by byte count, which is all ToolResultCap can do, throws
// away the signal and the noise in the same proportion. This throws away only
// noise.
//
// The rule that keeps it safe: a line matching a protected pattern is never
// dropped, whatever else applies. Errors, failures, warnings and panics survive
// even inside a run of identical lines, because "the same error 40 times" is
// itself the finding.
//
// Deliberately NOT a per-tool filter corpus. Recognising npm from cargo from go
// test means a rule pack per ecosystem, and rule packs need an evaluation
// harness before they can be trusted. What is here works on any command output
// because it targets transport ceremony, not semantics.
type ToolNoise struct {
	// MinRun is how many consecutive identical lines must appear before they are
	// collapsed into one plus a count.
	MinRun int
}

func (ToolNoise) Name() string { return "toolnoise" }

var (
	// ansiPattern covers CSI sequences (colour, cursor movement) and the OSC
	// title-setting sequences some tools emit.
	// Pattern adapted from github.com/acarl005/stripansi (MIT). It is battle
	// tested against the shapes a hand-rolled version tends to miss until it
	// meets one in the wild: OSC strings terminated by BEL, and the
	// private-parameter forms. Only the ESC introducer is matched here; the
	// 8-bit CSI form (U+009B) is vanishingly rare in tool output and would cost
	// a UTF-8 decode on every line to catch.
	ansiPattern = regexp.MustCompile("[\x1b]" + `[[\]()#;?]*(?:(?:(?:[a-zA-Z\d]*(?:;[a-zA-Z\d]*)*)?` + "\x07" + `)|(?:(?:\d{1,4}(?:;\d{0,4})*)?[\dA-PRZcf-ntqry=><~]))`)
	// protectedPattern is what must never be dropped. Matched case-insensitively
	// against the line.
	protectedPattern = regexp.MustCompile(`(?i)\b(error|fail(ed|ure|s)?|panic|fatal|warn(ing)?|exception|traceback|assert|denied|refused|timeout)\b`)
)

func (t ToolNoise) Apply(messages []Message) ([]Message, int) {
	minRun := t.MinRun
	if minRun < 3 {
		minRun = 3
	}

	out := clone(messages)
	saved := 0
	segments := make([]segment, 0, 4)
	for i, m := range out {
		segments = appendToolResultSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			text := seg.Text
			// Never touch valid JSON. This engine works line by line, and collapsing
			// a run of identical lines inside a JSON document silently produces
			// invalid JSON — the exact failure ToolResultCap already refuses. A
			// pretty-printed array of similar records is ordinary tool output, so
			// this is not a corner case.
			//
			// JSONTable is the engine that may rewrite JSON, and it earns that by
			// parsing the document and re-emitting it, rather than editing its text.
			if json.Valid([]byte(text)) {
				continue
			}
			// Two more structured shapes this engine must not collapse, both found
			// by adversarial review of the first version:
			//
			// A JSONTable artifact. Its rows are frequently identical, and
			// collapsing them leaves a header claiming N rows above a body carrying
			// one — the exact lie the table clip in ToolResultCap exists to
			// prevent. Its output is not valid JSON, so the guard above never
			// protected it.
			//
			// NDJSON. One JSON value per line fails json.Valid as a whole while
			// being structured records, so the reasoning behind the JSON guard
			// applies verbatim and the guard simply was not reaching it.
			if isTableArtifact(text) || looksLikeNDJSON(text) {
				continue
			}
			cleaned := stripNoise(text, minRun)
			if len(cleaned) >= len(text) {
				continue
			}
			m = seg.replace(m, cleaned)
			saved += len(text) - len(cleaned)
		}
		out[i] = m
	}
	if saved == 0 {
		return messages, 0
	}
	return out, saved
}

func stripNoise(text string, minRun int) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))

	flushRun := func(line string, count int) {
		kept = append(kept, line)
		if count > 1 {
			kept = append(kept, fmt.Sprintf("… [previous line repeated %d more times]", count-1))
		}
	}

	// flush decides a finished run: collapse it, unless the line is protected or
	// the run is too short. The protected check happens ONCE per run rather than
	// once per line — it is a regexp, and a run of 2000 identical lines used to
	// pay for 2000 of them.
	flush := func(line string, count int) {
		if count >= minRun && !protectedPattern.MatchString(line) {
			flushRun(line, count)
			return
		}
		for i := 0; i < count; i++ {
			kept = append(kept, line)
		}
	}

	runLine, runCount := "", 0
	for _, line := range lines {
		// A carriage return means the terminal redrew this line in place: only
		// the final frame was ever visible, so the earlier ones are not output,
		// they are animation.
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		// Fast path. The ANSI pattern is expensive — Go's regexp on a line that
		// cannot possibly match still walks it — and the overwhelming majority
		// of tool output has no escape at all. One IndexByte per line replaces a
		// regexp per line, which is where nearly all of this engine's cost was:
		// it ran 31x slower than the json.Marshal the caller pays anyway.
		if strings.IndexByte(line, 0x1b) >= 0 {
			line = ansiPattern.ReplaceAllString(line, "")
		}

		switch {
		case runCount == 0:
			runLine, runCount = line, 1
		case line == runLine:
			// A repeat. Whether it may be collapsed is decided once, when the run
			// ends: a repeated error line is kept in full, because forty of the
			// same failure is the finding, not the noise.
			runCount++
		default:
			flush(runLine, runCount)
			runLine, runCount = line, 1
		}
	}
	if runCount > 0 {
		flush(runLine, runCount)
	}
	return strings.Join(kept, "\n")
}

// isTableArtifact reports whether text was written by JSONTable. Recognising a
// sibling engine's output is not a layering violation: this engine's decision is
// "may these lines be collapsed", and for a table the answer is no, whatever
// produced it.
func isTableArtifact(text string) bool {
	return strings.HasPrefix(text, tableHeaderPrefix)
}

// looksLikeNDJSON reports whether text is one JSON value per line. Checked over
// the first few lines only: the question is what SHAPE this is, and a document
// that starts as NDJSON and stops being it partway is not something this engine
// should be collapsing either.
func looksLikeNDJSON(text string) bool {
	checked := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			return false
		}
		checked++
		if checked == 4 {
			return true
		}
	}
	// Fewer than four non-empty lines: two is already a repeated structure worth
	// protecting, and a single JSON line would have been caught by json.Valid.
	return checked >= 2
}
