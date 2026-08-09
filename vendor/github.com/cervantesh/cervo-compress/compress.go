// Package compress shrinks a chat request's message history without changing
// what the model can know.
//
// It is a LIBRARY on purpose. Deciding what may be dropped from a conversation
// needs knowledge of that conversation — which tool result still matters, what
// the user is actually doing, whether a block can be fetched back. A gateway
// sees a stateless snapshot and has none of that. So this package knows HOW to
// compress and holds no opinion about WHEN: the caller that owns the
// conversation decides, and that caller is a client, not a proxy.
//
// Everything here is deterministic, dependency-free and pure Go: same input,
// same output, no ML runtime, no network, no clock.
package compress

import "strings"

// Engine is one compression pass over a message history. An engine must:
//
//   - never mutate the input (callers share these maps);
//   - be safe to skip (returning the input unchanged is always valid);
//   - report honestly how many bytes it saved.
//
// Returning (input, 0) is the correct response to anything an engine is not
// sure about. Doing nothing is always a valid outcome; guessing is not.
type Engine interface {
	Name() string
	Apply(messages []Message) (out []Message, savedBytes int)
}

// Message is one entry in a chat history, in the OpenAI-compatible shape.
//
// Content is deliberately `any`: it is a string in the chat-completions dialect
// and an array of typed blocks in the Anthropic one. Engines do not deal with
// that themselves — segments.go turns both into the same list of addressable
// tool-result texts, and a block whose shape is not recognised is skipped
// rather than coerced.
type Message struct {
	Role    string
	Content any
	// Extra carries every other field on the original message so a round trip
	// through this package never drops anything it did not understand.
	Extra map[string]any
}

// Text returns the message content when it is plain text. Structured content
// (Anthropic blocks) returns false: see contentSegments for the view that
// covers both dialects.
func (m Message) Text() (string, bool) {
	s, ok := m.Content.(string)
	return s, ok
}

// EngineSaving is what one engine contributed to a pipeline.
type EngineSaving struct {
	Name       string
	SavedBytes int
}

// Report is what a pipeline achieved, for logging and for telling the user.
type Report struct {
	OriginalBytes int
	// SavedBytes is the total, and is exactly Size(in) - Size(out).
	SavedBytes int
	// Engines names the engines that fired, in the order they fired.
	Engines []string
	// ByEngine is the same list with each engine's own contribution. A total
	// alone cannot answer the only question an operator actually has — "what
	// does turning this one off cost me?" — because the answer is anywhere
	// between nothing and all of it.
	//
	// The numbers are sequential, not independent: each engine saw what the
	// ones before it left. Reordering the pipeline moves the credit around, so
	// these are attributions within a run, not standalone engine scores.
	ByEngine []EngineSaving
}

// SavedBy totals what one engine contributed, by name. Zero if it never fired.
func (r Report) SavedBy(name string) int {
	total := 0
	for _, e := range r.ByEngine {
		if e.Name == name {
			total += e.SavedBytes
		}
	}
	return total
}

// Pipeline runs engines in order and reports the total.
//
// A pipeline that saves nothing returns the ORIGINAL slice, not a copy: callers
// can compare identity to know that nothing happened.
func Pipeline(messages []Message, engines ...Engine) ([]Message, Report) {
	report := Report{OriginalBytes: Size(messages)}
	current := messages
	for _, engine := range engines {
		next, saved := engine.Apply(current)
		if saved <= 0 {
			continue
		}
		current = next
		report.SavedBytes += saved
		report.Engines = append(report.Engines, engine.Name())
		report.ByEngine = append(report.ByEngine, EngineSaving{Name: engine.Name(), SavedBytes: saved})
	}
	if report.SavedBytes <= 0 {
		return messages, report
	}
	return current, report
}

// Size is the byte length of every textual content in the history. It is bytes,
// not tokens: counting tokens honestly would need a per-model tokenizer, and a
// wrong token count is worse than an admitted byte count.
//
// It counts exactly what an engine can address — plain content plus tool-result
// text inside structured blocks — so that Size(in) - Size(out) is always the
// saving a pipeline reports. Envelope bytes (roles, block types, tool_use_ids)
// are not counted: no engine can shrink them, so including them would only
// flatter the ratio.
func Size(messages []Message) int {
	total := 0
	var segments []segment
	for _, m := range messages {
		// Fast path: the overwhelmingly common shape, and the one that must not
		// pay for dialect support it does not use.
		if text, ok := m.Text(); ok {
			total += len(text)
			continue
		}
		// Everything is counted, including segments too large for any engine to
		// touch: Size measures the history, not the part of it we are willing
		// to work on.
		segments = appendToolResultSegments(segments[:0], m, unlimited)
		for _, s := range segments {
			total += len(s.Text)
		}
	}
	return total
}

// clone copies the slice so an engine can replace whole messages freely. It is
// deliberately shallow: what a message points AT — the block arrays and maps of
// the structured dialects — is still the caller's, which is why replacements go
// through segment.replace, and why nothing here writes into a block in place.
func clone(messages []Message) []Message {
	out := make([]Message, len(messages))
	copy(out, messages)
	return out
}

// Recommended is the engine order the evaluation corpus supports, given a byte
// budget per tool result.
//
// The order is not arbitrary and not a matter of taste. The harness showed that
// running the lossless engines first preserves meaning that running the lossy
// one first destroys: on a realistic npm install, ToolResultCap alone loses the
// EBUSY error because it sits in the middle and the cap keeps the two ends, and
// the same budget keeps it once ToolNoise has removed the ceremony crowding it
// out.
//
// Stated generally: every lossless engine belongs in front of every lossy one. A
// byte budget spent carrying noise is a byte budget not spent on the answer.
//
// CommandClass sits between the two groups, which is where the same argument
// puts it. It is lossy — it deletes lines outright — so it goes behind Dedup,
// whose content never leaves the request. But it deletes only what it recognises,
// while the cap deletes by byte offset whatever happens to be in the middle, so
// it goes in FRONT of the cap for the reason the npm finding gives: a budget
// spent on `=== RUN` lines and spinner frames is a budget not spent on the
// failure.
//
// JSONTable is the exception, and it goes LAST for a reason the harness also
// found. Its output is a text table, deliberately not valid JSON — which
// silently disarms every guard keyed on json.Valid. Run it early and the cap
// stops recognising structured data it was refusing a moment before, so on a
// JSON array the recommended order used to preserve LESS than putting the cap
// first. Running it last removes the hazard by construction: nothing downstream
// can be confused by an artifact nothing downstream sees.
func Recommended(toolResultLimit int) []Engine {
	return []Engine{
		ToolNoise{MinRun: 3},                  // lossless-ish: removes only ceremony
		Dedup{MinBytes: 256},                  // lossless: the content still travels
		CommandClass{},                        // lossy, but only where it recognises the producer
		ToolResultCap{Limit: toolResultLimit}, // lossy and blind: what the others could not shrink
		JSONTable{MinRows: 5},                 // lossless restructuring, LAST — see below
	}
}

// Budget runs engines only until the history fits, and stops.
//
// Compressing past the point of fitting is pure downside: every further engine
// can only lose more meaning for a saving nobody asked for. This runs them in
// order and returns as soon as Size drops to or below maxBytes, so on a history
// that was already small the answer is "did nothing", and on one that needs a
// little help only the cheapest, safest engines run.
//
// With maxBytes <= 0 there is no target and every engine runs, which is what
// Pipeline does.
func Budget(messages []Message, maxBytes int, engines ...Engine) ([]Message, Report) {
	if maxBytes <= 0 {
		return Pipeline(messages, engines...)
	}
	report := Report{OriginalBytes: Size(messages)}
	if report.OriginalBytes <= maxBytes {
		return messages, report
	}
	current := messages
	for _, engine := range engines {
		next, saved := engine.Apply(current)
		if saved <= 0 {
			continue
		}
		current = next
		report.SavedBytes += saved
		report.Engines = append(report.Engines, engine.Name())
		report.ByEngine = append(report.ByEngine, EngineSaving{Name: engine.Name(), SavedBytes: saved})
		if Size(current) <= maxBytes {
			break // it fits; anything more would be loss without purpose
		}
	}
	if report.SavedBytes <= 0 {
		return messages, report
	}
	return current, report
}

// Text is every piece of readable text in a history, concatenated, in either
// dialect.
//
// It exists because a caller inspecting what survived compression must not have
// to know which wire shape it is looking at. Reading Content.(string) directly
// works on the OpenAI dialect and silently returns nothing on the Anthropic one
// — which does not fail loudly, it reports that everything was lost. The
// evaluation harness had exactly that bug, and it made the Anthropic dialect
// look broken while the engines were fine.
//
// Size is the length of what this returns, less the separators.
func Text(messages []Message) string {
	var b strings.Builder
	var segments []segment
	for _, m := range messages {
		if text, ok := m.Text(); ok {
			b.WriteString(text)
			b.WriteString("\n")
			continue
		}
		segments = appendToolResultSegments(segments[:0], m, unlimited)
		for _, s := range segments {
			b.WriteString(s.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}
