package compress

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CommandClass strips what a RECOGNISED command emits as ceremony, keeping what
// the same command emits as signal.
//
// ToolNoise is deliberately semantics-blind: it targets transport ceremony —
// ANSI, carriage-return redraws, runs of identical lines — because that is true
// of any command. It therefore saves nothing at all on the three shapes an agent
// meets most often. A `go test -v` run has no repeated lines and no escapes: it
// is 78 lines of which two matter, and every one of them is distinct. A stack
// trace is the same. A `git status` likewise. On the evaluation corpus ToolNoise
// saves 0.0% on all three.
//
// This engine closes that gap by asking a question ToolNoise refuses to ask:
// WHAT PRODUCED THIS? Knowing the producer means knowing that `=== RUN` is an
// announcement of work rather than a result, that a `reify:` line is a progress
// channel, that a frame in net/http is the runtime unwinding rather than the bug.
//
// # Why this is dangerous, and what makes it safe
//
// Every rule here is a claim about meaning, and a rule pack applied to the wrong
// output is not merely a worse compression — it deletes the answer while looking
// like it worked. Four rules hold it down:
//
//  1. DETECTION IS CONSERVATIVE, AND AMBIGUITY MEANS DO NOTHING. A class must
//     match several distinct structural signals, not one keyword. If two classes
//     both claim the text — a test run that panics is the ordinary example — the
//     engine does nothing at all, because which pack is right is exactly the
//     question it may not guess at. Doing nothing is always a valid answer.
//
//  2. THE PROTECTED PATTERN OUTRANKS EVERY RULE. Errors, failures, warnings,
//     panics and timeouts survive whatever a rule pack says about their line.
//     The pack decides what is boring; it does not get to decide what is
//     alarming. This is the same protectedPattern ToolNoise uses, on purpose:
//     one definition of "must not be dropped" for the whole package.
//
//  3. STRUCTURED DATA IS NEVER TOUCHED. Valid JSON, NDJSON, and a JSONTable
//     artifact are all skipped, for the reasons those guards were written for
//     elsewhere in this package. A line-level engine editing a document another
//     engine has to parse is where four of the last review's defects came from.
//
//  4. REMOVALS ARE DECLARED. A dropped run leaves a marker saying how many lines
//     went and which pack took them, so a reader can tell a filtered result from
//     a complete one — the property that separates lossy from dangerous.
//
// # Adding a class
//
// A class is data: a name, the literals that make it worth testing for, the
// signals that identify it, and a list of line rules. It is one declaration in
// classes, and nothing else in this file needs to change. That is deliberate —
// the moment recognising a tool means writing control flow, the rules stop being
// reviewable as a set.
//
// # What is NOT here
//
// Only classes that the evaluation harness shows beating ToolNoise on their own
// output while preserving every declared property. A pack that is not measurably
// better than the generic engine is pure risk, and yarn and pnpm are absent for
// exactly that reason: there is no corpus entry for either, so shipping rules for
// them would be a bet, and this package's whole claim is that it does not bet.
type CommandClass struct {
	// MinLines is the length below which nothing is classified. A three-line
	// output that happens to contain one recognisable phrase has no ceremony to
	// remove and no context to be sure with, so guessing at it is all downside.
	MinLines int
}

func (CommandClass) Name() string { return "command-class" }

// classMarker declares what a rule pack removed. It names the pack as well as
// the count, because "8 lines removed" invites the reader to wonder what they
// were, and "8 lines removed: go test progress" answers it.
//
// It is kept SHORT on purpose, and the harness is why. A marker is only emitted
// when it costs less than the lines it replaces, so its length is a floor on
// what this engine can remove: at the first, more descriptive wording — "git
// porcelain hints and transfer progress" — a marker ran to 62 bytes and every
// isolated hint line in a `git status` was cheaper to keep than to announce, so
// the git pack removed almost nothing. The description belongs in the class
// declaration, where it is read once; the marker only has to name the pack.
const classMarker = "… [%d lines removed: %s]"

// classMarkerOne is the same notice for a single line. "1 lines removed" is the
// sort of small wrongness that makes a reader distrust the rest of the output.
const classMarkerOne = "… [1 line removed: %s]"

func removalNotice(lines int, what string) string {
	if lines == 1 {
		return fmt.Sprintf(classMarkerOne, what)
	}
	return fmt.Sprintf(classMarker, lines, what)
}

// classMarkerPrefix is the invariant head of both this engine's marker and
// ToolNoise's. No rule may drop a line that starts with it: a marker explaining
// a removal is not itself removable, and a second pass over the same text has to
// be a no-op.
const classMarkerPrefix = "… ["

// dropRule is one line-level decision inside a class.
type dropRule struct {
	// Why is the justification, written where the rule is. A rule pack is
	// reviewed by reading it, and a bare regexp cannot be argued with.
	Why string
	// Match is tested against the whole line.
	Match *regexp.Regexp
	// Continuation also drops the tab-indented lines that follow a match.
	//
	// It exists because a Go stack frame is TWO lines — the call, and where it
	// was made — and dropping one without the other leaves a file position with
	// nothing to position. Tab indentation is the continuation convention in
	// every format this engine recognises.
	Continuation bool
}

// commandClass is one recognised producer and the rules for its output.
type commandClass struct {
	// Name identifies the pack.
	Name string
	// What is the phrase the removal marker uses. It must read as a description
	// of what went, to a reader who cannot see it.
	What string
	// Hints are cheap literal substrings, at least one of which must appear
	// before the signals are worth running. Detection is otherwise a dozen
	// regexps over every tool result in the history, most of which cannot
	// possibly match; one Contains per class replaces that.
	Hints []string
	// Signals are the structural shapes that identify the class. They are
	// deliberately about STRUCTURE — line prefixes, the layout of a summary
	// line — rather than about vocabulary, because a word can appear in prose
	// that quotes it and a layout cannot.
	Signals []*regexp.Regexp
	// MinSignals is how many DISTINCT signals must fire. Above one, always: a
	// single matching line is a coincidence, and a coincidence that hands the
	// wrong rule pack a result is the failure mode that makes this engine
	// dangerous rather than merely lossy.
	MinSignals int
	// Drop is the rule pack, tried in order.
	Drop []dropRule
}

// classes is the whole configuration of this engine.
//
// Each entry says what it recognises, how sure it has to be, and what it takes
// out — and each rule says why. Read top to bottom, this is the complete list of
// claims the engine makes about meaning.
var classes = []commandClass{
	{
		Name: "go-test",
		What: "go test progress",
		Hints: []string{
			"=== RUN", "--- PASS:", "--- FAIL:", "[no test files]",
		},
		Signals: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^=== RUN\s`),
			regexp.MustCompile(`(?m)^\s*--- (PASS|FAIL|SKIP): \S`),
			// A per-package verdict: name then elapsed time or a cache hit.
			regexp.MustCompile(`(?m)^(ok|FAIL)\s+\S+\s+(\(cached\)|[0-9.]+m?s)`),
			regexp.MustCompile(`(?m)^\?\s+\S+\s+\[no test files\]`),
			regexp.MustCompile(`(?m)^(PASS|FAIL)$`),
		},
		MinSignals: 3,
		Drop: []dropRule{
			{
				Why:   "the runner announces every test before it runs it, and the result lines below already name every one of them",
				Match: regexp.MustCompile(`^=== (RUN|PAUSE|CONT|NAME)\s`),
			},
			{
				Why:   "a passing test's body is not why anyone reads a test run; the verdict lines still say the package passed",
				Match: regexp.MustCompile(`^\s*--- PASS: \S`),
			},
			{
				Why:   "a bare PASS restates the per-package ok lines directly beneath it",
				Match: regexp.MustCompile(`^PASS$`),
			},
			{
				Why:   "a package with no test files has nothing to report about",
				Match: regexp.MustCompile(`^\?\s+\S+\s+\[no test files\]`),
			},
			{
				Why:   "coverage percentages answer a different question from the one a failing run raises",
				Match: regexp.MustCompile(`^(coverage:\s+\d|\s*total:\s+\(statements\))`),
			},
		},
	},
	{
		Name: "npm",
		// "ceremony" rather than "progress": the pack also takes the funding
		// appeal and the version nag, and a marker that misdescribes what it
		// removed is worse than a vaguer one that does not.
		What: "npm ceremony",
		Hints: []string{
			"npm warn", "npm error", "npm notice", "npm ERR!", "reify:", "npm fund",
		},
		Signals: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^npm (warn|error|notice|http|ERR!|WARN) `),
			regexp.MustCompile(`(?m)^added \d+ packages`),
			regexp.MustCompile(`(?m)^\d+ packages are looking for funding`),
			regexp.MustCompile("(?m)^\\s*run `npm (fund|audit)"),
			regexp.MustCompile(`(?m)^(\S+ )?(reify|idealTree|timing):`),
			regexp.MustCompile(`(?m)^\d+ \S+ severity vulnerabilit`),
		},
		MinSignals: 3,
		Drop: []dropRule{
			{
				// The braille block is the spinner alphabet npm draws with. A line
				// whose first character is one of them is a frame of animation:
				// only the last frame was ever on screen, and ToolNoise cannot
				// collapse them because every frame differs in that first glyph.
				Why:   "a spinner frame is animation that was redrawn in place, not output",
				Match: regexp.MustCompile(`^[\x{2800}-\x{28FF}]\s`),
			},
			{
				Why:   "the reify/idealTree channel is per-package progress; what was installed is in the summary",
				Match: regexp.MustCompile(`^(\S+ )?(reify|idealTree|timing):`),
			},
			{
				Why:   "npm http fetch lines are one per request and say nothing the summary does not",
				Match: regexp.MustCompile(`^npm http fetch `),
			},
			{
				Why:   "the funding appeal is addressed to a human with a wallet",
				Match: regexp.MustCompile("^(\\d+ packages are looking for funding|\\s*run `npm fund` for details)$"),
			},
			{
				Why:   "npm notice is the version nag; it is never about this install",
				Match: regexp.MustCompile(`^npm notice `),
			},
		},
	},
	{
		Name: "git",
		// Most of what this pack removes from a `git status` is porcelain hints
		// rather than progress, so the marker says neither and covers both.
		What: "git ceremony",
		Hints: []string{
			"On branch ", "diff --git ", "Changes to be committed:", "(use \"git ",
			"CONFLICT (", "objects:", "Automatic merge failed",
		},
		Signals: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^On branch \S`),
			regexp.MustCompile(`(?m)^diff --git a/\S`),
			regexp.MustCompile(`(?m)^(Changes to be committed|Changes not staged for commit|Untracked files|Unmerged paths):$`),
			regexp.MustCompile(`(?m)^\t(modified|new file|deleted|renamed|both modified|both added):`),
			regexp.MustCompile(`(?m)^(remote: )?(Enumerating|Counting|Compressing|Writing|Receiving|Resolving|Unpacking) (objects|deltas|files)`),
			regexp.MustCompile(`(?m)^CONFLICT \(`),
			regexp.MustCompile(`(?m)^Automatic merge failed`),
			regexp.MustCompile(`(?m)^ \d+ files? changed`),
			regexp.MustCompile(`(?m)^\s*\(use "git `),
		},
		MinSignals: 3,
		Drop: []dropRule{
			{
				Why:   "the parenthesised hints are addressed to a human at a terminal deciding what to type next",
				Match: regexp.MustCompile(`^\s*\(use "git [^"]*"[^)]*\)$`),
			},
			{
				Why:   "object counting and transfer percentages are a progress bar; the file list and the summary say what moved",
				Match: regexp.MustCompile(`^(remote: )?(Enumerating|Counting|Compressing|Writing|Receiving|Resolving|Unpacking) (objects|deltas|files)`),
			},
			{
				Why:   "the delta/reuse totals are transport accounting, not content",
				Match: regexp.MustCompile(`^(remote: )?Total \d+ \(delta \d+\)`),
			},
			{
				Why:   "an empty remote: line is a blank the server sent to space its own progress bar",
				Match: regexp.MustCompile(`^remote:\s*$`),
			},
		},
	},
	{
		Name: "go-stack",
		What: "go runtime frames",
		Hints: []string{
			"goroutine ", "panic: ", "created by ", "fatal error: ",
		},
		Signals: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^panic: `),
			regexp.MustCompile(`(?m)^fatal error: `),
			regexp.MustCompile(`(?m)^goroutine \d+ \[[^\]]*\]:$`),
			regexp.MustCompile(`(?m)^\t\S*\.go:\d+`),
			regexp.MustCompile(`(?m)^created by \S+ in goroutine \d+$`),
			regexp.MustCompile(`(?m)^\[signal SIG`),
		},
		MinSignals: 3,
		Drop: []dropRule{
			{
				// A frame is dropped by the PACKAGE PATH of its call, not by the
				// file path of its location: an import path with no dot before the
				// first slash is by definition not a module, so it is the standard
				// library or the runtime. That is a property of the language's
				// naming, which is why this reads as a list and is still a rule
				// rather than a guess.
				//
				// Only the deep end goes. The frames that locate the bug are the
				// caller's own packages, and every one of those is kept — including
				// main, which is where a crash is usually read from.
				Why:          "frames in the runtime and the standard library are the language unwinding, not the code that broke",
				Match:        regexp.MustCompile(`^(created by )?(runtime|internal|net|os|syscall|sync|time|context|io|bufio|bytes|strings|strconv|sort|reflect|fmt|errors|encoding|crypto|compress|database|path|regexp|math|text|html|log|hash|mime|unicode|testing|slices|maps|cmp)([./][\w./]*)?[.(]`),
				Continuation: true,
			},
		},
	},
}

func (c CommandClass) Apply(messages []Message) ([]Message, int) {
	minLines := c.MinLines
	if minLines < 6 {
		minLines = 6
	}

	out := clone(messages)
	saved := 0
	segments := make([]segment, 0, 4)
	for i, m := range out {
		segments = appendToolResultSegments(segments[:0], m, MaxSegmentBytes)
		for _, seg := range segments {
			text := seg.Text
			class, ok := classify(text, minLines)
			if !ok {
				continue
			}
			// The three shapes no line-level engine may edit, for the reasons
			// ToolNoise documents at length: a JSON document another engine has
			// to parse, a stream of them, and a table whose header states a row
			// count its body has to match. This engine is line-level in exactly
			// the same way, so it inherits exactly the same prohibition.
			//
			// The guard runs AFTER classification, not before, and only because
			// it is expensive: json.Valid copies the whole segment to scan it,
			// and this engine sees every tool result in a history while claiming
			// almost none of them. Refusing a class it never chose costs nothing
			// and buys nothing, so the order is a saving with no meaning attached
			// — the check still gates every edit.
			if json.Valid([]byte(text)) || isTableArtifact(text) || looksLikeNDJSON(text) {
				continue
			}
			cleaned := applyClass(class, text)
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

// classify names the single class that recognises this output, if there is
// exactly one.
//
// Exactly one is the whole safety argument. A `go test` run that ends in a panic
// carries the signals of both the test class and the stack class, and the two
// packs disagree about the same lines. There is no principled way to pick from
// here, so nothing is picked: the result travels uncompressed and whole, which is
// worse than a good answer and much better than a confident wrong one.
func classify(text string, minLines int) (commandClass, bool) {
	if strings.Count(text, "\n")+1 < minLines {
		return commandClass{}, false
	}
	var found commandClass
	matched := 0
	for _, class := range classes {
		if !class.plausible(text) || !class.detects(text) {
			continue
		}
		matched++
		if matched > 1 {
			return commandClass{}, false
		}
		found = class
	}
	return found, matched == 1
}

// plausible is the cheap literal gate in front of the regexps.
func (c commandClass) plausible(text string) bool {
	for _, hint := range c.Hints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

// detects reports whether enough DISTINCT signals fire. Counting distinct
// patterns rather than matching lines is the point: a thousand lines of one
// shape is one piece of evidence, not a thousand.
func (c commandClass) detects(text string) bool {
	distinct := 0
	for _, signal := range c.Signals {
		if !signal.MatchString(text) {
			continue
		}
		distinct++
		if distinct >= c.MinSignals {
			return true
		}
	}
	return false
}

// applyClass runs one pack over one tool result.
//
// Dropped lines are accumulated rather than discarded, because a run is only
// worth removing if the marker that declares it costs less than the lines did.
// A single dropped line almost never pays, and silently keeping it is better
// than paying fifty bytes to announce the removal of forty.
func applyClass(c commandClass, text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))

	var pending []string
	pendingBytes := 0

	flush := func() {
		if len(pending) == 0 {
			return
		}
		marker := removalNotice(len(pending), c.What)
		if len(marker) < pendingBytes {
			kept = append(kept, marker)
		} else {
			kept = append(kept, pending...)
		}
		pending = pending[:0]
		pendingBytes = 0
	}
	drop := func(line string) {
		pending = append(pending, line)
		pendingBytes += len(line) + 1 // the newline that followed it
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		rule, dropIt := c.ruleFor(line)
		if !dropIt {
			flush()
			kept = append(kept, line)
			continue
		}
		drop(line)
		if !rule.Continuation {
			continue
		}
		for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t") &&
			!protectedPattern.MatchString(lines[i+1]) {
			i++
			drop(lines[i])
		}
	}
	flush()
	return strings.Join(kept, "\n")
}

// ruleFor is the decision about one line, in the order the safety argument runs:
// a marker is never removable, a protected line outranks every rule, and only
// then does the pack get a say.
func (c commandClass) ruleFor(line string) (dropRule, bool) {
	if line == "" || strings.HasPrefix(line, classMarkerPrefix) {
		return dropRule{}, false
	}
	if protectedPattern.MatchString(line) {
		return dropRule{}, false
	}
	for _, rule := range c.Drop {
		if rule.Match.MatchString(line) {
			return rule, true
		}
	}
	return dropRule{}, false
}
