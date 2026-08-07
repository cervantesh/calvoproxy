package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// routeTrace records WHY a request was served by the model that served it.
//
// setServedModelHeaders already answers "which model" — see the incident in its
// doc comment. It cannot answer "why that one": the models skipped for an open
// circuit, the ones that failed first and with what code, the score the chain
// was ordered by. Today that lives only in the "Resolving Route" slog line in
// dispatchChain and never reaches the caller.
//
// Ownership: exactly ONE goroutine writes a trace — the one serving the request.
// It is therefore lock-free by construction. The stream goroutines (streamCopy,
// awaitFirstStreamEvent) must never touch it; the stream outcome is consolidated
// back in the request goroutine, after the headers are already committed, which
// is why it can never appear in the header. See docs/specs/P1-decision-trace.md.
type routeTrace struct {
	ID      string
	Profile string
	// RequestedModel is what the client pinned, "" when it pinned nothing. It
	// comes straight off the wire, so it is sanitised before it can reach a
	// header value (spec §6).
	RequestedModel string
	// RuleID is the policy rule that authorised this route, and PolicyReason the
	// text that rule carries. PolicySteps is the engine's own evaluation trace:
	// which rules were considered and which matched.
	//
	// These three answer a question the rest of the trace cannot: the chain
	// section explains why a MODEL was picked from the chain, but not why THIS
	// chain. Without the rule id, a request routed to the wrong profile looks
	// identical to one routed correctly.
	RuleID       string
	PolicyReason string
	PolicySteps  []policyTraceStep
	// Outcome is empty while the request is still in flight and outcomeServed
	// once a model answered. The other values name a request that never reached
	// a model at all — precisely the ones where the caller most needs a reason,
	// since an HTTP status alone cannot tell "everything is cooling down" from
	// "nothing here can do vision".
	Outcome string

	CapsRequired []string
	// How the chain narrowed, stage by stage: planned by policy, left after the
	// capability filter, left after the breaker and the MaxAttempts truncation.
	Planned, AfterCaps, Eligible int
	// ExcludedByBreaker counts models the breaker gated out. A chain that keeps
	// shrinking is the difference between "this model is slow" and "half the
	// chain is in cooldown".
	ExcludedByBreaker int
	// ExcludedByQuota counts models gated out because their budget window is
	// spent. Kept apart from ExcludedByBreaker on purpose: "broken right now" and
	// "out of allowance until midnight" call for completely different actions,
	// and folding them together is exactly the ambiguity this trace exists to
	// remove.
	ExcludedByQuota int

	Attempts []traceAttempt
	Served   *traceAttempt

	// compression is what P3 achieved on this request. cmp= is emitted always,
	// so "not compressed" stays distinguishable from "this field does not exist".
	compression compressionStat

	// scores is written once, when the chain is ranked, and read when an attempt
	// is recorded. Never exported: it is a rendering detail.
	scores map[string]float64
}

// traceAttempt is one model's turn in the chain. Reason is upstream error text
// and therefore NEVER leaves through the header or the client-facing JSON; see
// spec §6.
type traceAttempt struct {
	Model  string
	Index  int
	Score  float64
	Status int
	Kind   string
	Reason string
}

// policyTraceStep is one rule the policy engine evaluated, flattened from
// cervorules.DecisionTraceStep so the router's trace does not leak the engine's
// types into its JSON shape.
//
// Detail is the engine's per-step text. It is NOT upstream error body — it is
// authored by the policy, which is generated from this repo's own config — so
// the §6 rule that keeps traceAttempt.Reason admin-only does not apply to it for
// that reason. It is admin-only anyway, for a different one: it describes the
// internals of the authorisation rules to a caller that has passed no gate.
// Name and Matched are closed, config-derived identifiers and travel further.
type policyTraceStep struct {
	Name    string
	Matched bool
	Detail  string
}

// recordPolicy stamps the authorisation decision on the trace. Called from
// dispatchChain rather than from authorizeOperationalRoute: the policy runs
// BEFORE the trace exists (the decision is what selects the chain, so it cannot
// wait for it), and the decision struct is the only thing that crosses between
// them. Threading it that way beats moving newRouteTrace earlier, which would
// have to be repeated at all four authorize call sites — two of which never
// reach dispatchChain at all — and would mint trace ids for requests that have
// no chain to trace.
func (t *routeTrace) recordPolicy(ruleID, reason, requestedModel string, steps []policyTraceStep) {
	if t == nil {
		return
	}
	t.RuleID, t.PolicyReason, t.RequestedModel = ruleID, reason, requestedModel
	t.PolicySteps = steps
}

func (t *routeTrace) recordChain(planned, afterCaps, eligible int, required []string) {
	if t == nil {
		return
	}
	t.Planned, t.AfterCaps, t.Eligible = planned, afterCaps, eligible
	t.CapsRequired = required
	t.ExcludedByBreaker = afterCaps - eligible - t.ExcludedByQuota
	if t.ExcludedByBreaker < 0 {
		t.ExcludedByBreaker = 0
	}
}

// recordScores stores the scores the chain was ordered by, captured where
// rankAttemptsByScore already computed them — no second pass over the chain.
// recordQuotaExclusions must be called BEFORE recordChain, which derives the
// breaker count by subtraction and needs to know how much of the drop was budget
// rather than health.
func (t *routeTrace) recordQuotaExclusions(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.ExcludedByQuota = n
}

// recordCompression stores what the engines achieved, for the cmp= field.
func (t *routeTrace) recordCompression(stat compressionStat) {
	if t == nil {
		return
	}
	t.compression = stat
}

func (t *routeTrace) recordScores(models []string, scores []float64) {
	if t == nil || len(models) != len(scores) {
		return
	}
	t.scores = make(map[string]float64, len(models))
	for i, m := range models {
		t.scores[m] = scores[i]
	}
}

func (t *routeTrace) recordAttemptFailure(model string, index, status int, kind, reason string) {
	if t == nil {
		return
	}
	t.Attempts = append(t.Attempts, traceAttempt{
		Model: model, Index: index, Status: status,
		Kind: kind, Reason: truncateReason(reason), Score: t.scores[model],
	})
}

func (t *routeTrace) recordServed(model string, index int) {
	if t == nil {
		return
	}
	t.Outcome = outcomeServed
	t.Served = &traceAttempt{
		Model: model, Index: index, Status: http.StatusOK,
		Kind: "ok", Score: t.scores[model],
	}
}

const (
	outcomeServed      = "served"
	outcomeCapsPinned  = "caps_pinned"
	outcomeCapsNone    = "caps_none"
	outcomeAllCooling  = "all_cooling"
	outcomeChainFailed = "chain_failed"
)

type traceCtxKey struct{}

// traceEnabled gates the whole subsystem. Off means no trace is allocated at
// all, not a blanked header: a diagnostic must be removable from the picture
// entirely, and this is the escape hatch if it ever costs something on the hot
// path.
func traceEnabled() bool { return envBool("PROXY_ROUTE_TRACE", true) }

// newRouteTrace mints a trace with the id the client is handed as
// X-Calvoproxy-Decision-Id. Returns nil when tracing is off, which every
// annotation point already tolerates.
func newRouteTrace(profile string) *routeTrace {
	if !traceEnabled() {
		return nil
	}
	return &routeTrace{ID: newTraceID(), Profile: profile}
}

func newTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A trace id is diagnostic, never load-bearing: a request must not fail
		// because the entropy source hiccuped.
		return ""
	}
	return hex.EncodeToString(b[:])
}

func withTrace(ctx context.Context, trace *routeTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey{}, trace)
}

// traceFrom returns the request's trace, or nil when tracing is off or the
// caller is out of band (a direct executor call in a test). Every method on
// *routeTrace tolerates a nil receiver, so callers never need to check.
func traceFrom(ctx context.Context) *routeTrace {
	trace, _ := ctx.Value(traceCtxKey{}).(*routeTrace)
	return trace
}

const (
	traceHeader     = "X-Calvoproxy-Route"
	traceIDHeader   = "X-Calvoproxy-Decision-Id"
	traceVersion    = "v1"
	traceFieldSep   = ";"
	traceMaxHeader  = 512
	traceNoCompress = "off"
	// traceMaxRuleID bounds rid=. The rule id looks config-derived — it is
	// "route." + the operation — but the operation can come from the client's
	// X-Cervo-Capability header (cervohttpadapter takes it verbatim), so its
	// length is not ours to assume. Over the bound the field is OMITTED rather
	// than truncated: a truncated identifier is a wrong identifier, and rid= is
	// only worth carrying if a consumer can compare it to the policy.
	traceMaxRuleID = 64
)

// header renders the short form: the stable, always-on contract. Only codes and
// closed enumerations go in here — never upstream error text, which is what
// /decisions/{id} is for (spec §6).
func (t *routeTrace) header() string {
	if t == nil {
		return ""
	}
	// Protected fields, in order. Trimming never touches these: a trace that
	// cannot say which profile it is or whether it compressed is not a trace.
	protected := []string{
		traceVersion,
		"p=" + traceSanitize(t.Profile),
		// Always emitted, so "not compressed" stays distinguishable from "this
		// field does not exist" once P3 lands.
		"cmp=" + t.compression.header(),
	}
	// Only on the unserved paths: on a served request the outcome is implicit in
	// the presence of X-Calvoproxy-Model, and spending header bytes to repeat it
	// costs the fields that carry real information.
	if t.Outcome != "" && t.Outcome != outcomeServed {
		protected = append(protected, "o="+traceSanitize(t.Outcome))
	}
	if t.Served != nil {
		protected = append(protected, "s="+strconv.FormatFloat(t.Served.Score, 'f', 2, 64))
		if t.Served.Index > 1 {
			// The degraded signal: this answer came from a fallback.
			protected = append(protected, "a="+strconv.Itoa(t.Served.Index))
		}
	}
	if len(t.CapsRequired) > 0 {
		protected = append(protected, "caps="+traceSanitize(strings.Join(t.CapsRequired, "+")))
	}
	// rid= is additive and optional, per the v1 schema rule: a consumer that has
	// never heard of it parses the header exactly as before.
	//
	// It belongs on THIS channel, unlike the rest of what the policy produced,
	// because it is the one closed identifier among them — an opaque key a
	// consumer resolves against the policy it already has. PolicyReason and the
	// steps' Detail are free text of unbounded length; putting either here would
	// spend the 512-byte budget on prose and push out prev=, which is the field
	// that actually says what went wrong (spec §6).
	if rid := traceRuleID(t.RuleID); rid != "" {
		protected = append(protected, "rid="+rid)
	}

	// Droppable, in reverse order of value.
	shape := fmt.Sprintf("n=%d/%d/%d", t.Planned, t.AfterCaps, t.Eligible)
	brk := ""
	if t.ExcludedByBreaker > 0 {
		brk = "brk=" + strconv.Itoa(t.ExcludedByBreaker)
	}
	if t.ExcludedByQuota > 0 {
		if brk != "" {
			brk += ";"
		}
		brk += "q=" + strconv.Itoa(t.ExcludedByQuota)
	}
	prev := make([]string, 0, len(t.Attempts))
	for _, a := range t.Attempts {
		prev = append(prev, traceShortModel(a.Model)+":"+attemptCode(a))
	}

	return renderTraceHeader(protected, shape, brk, prev)
}

// renderTraceHeader assembles the header and, if it is over the cap, trims it
// deterministically: prev= entries from the end, then prev= entirely, then the
// chain shape. Anything trimmed is declared with trunc=1, so a consumer never
// reads a trimmed trace as a short chain.
func renderTraceHeader(protected []string, shape, brk string, prev []string) string {
	build := func(prev []string, withShape bool) (string, bool) {
		fields := append([]string{}, protected...)
		if withShape {
			if shape != "" {
				fields = append(fields, shape)
			}
			if brk != "" {
				fields = append(fields, brk)
			}
		}
		if len(prev) > 0 {
			fields = append(fields, "prev="+strings.Join(prev, ","))
		}
		return strings.Join(fields, traceFieldSep), true
	}

	if out, _ := build(prev, true); len(out) <= traceMaxHeader {
		return out
	}
	const truncated = traceFieldSep + "trunc=1"
	for n := len(prev) - 1; n >= 0; n-- {
		if out, _ := build(prev[:n], true); len(out)+len(truncated) <= traceMaxHeader {
			return out + truncated
		}
	}
	if out, _ := build(nil, false); len(out)+len(truncated) <= traceMaxHeader {
		return out + truncated
	}
	// Only the protected fields left. If those alone blow the cap it is a bug,
	// and the test for invariant 4 is what catches it.
	out, _ := build(nil, false)
	return out + truncated
}

// attemptCode renders an attempt's outcome for prev=: the HTTP status when
// there is one, otherwise a closed enumeration. Never upstream text.
func attemptCode(a traceAttempt) string {
	if a.Status > 0 {
		return strconv.Itoa(a.Status)
	}
	if a.Kind != "" {
		return traceSanitize(a.Kind)
	}
	return "err"
}

// traceRuleID renders the rule id for the header, or "" when it cannot be
// rendered honestly: empty, or longer than the bound once sanitised. See
// traceMaxRuleID for why absence beats truncation.
func traceRuleID(ruleID string) string {
	if ruleID == "" {
		return ""
	}
	sanitized := traceSanitize(ruleID)
	if len(sanitized) > traceMaxRuleID {
		return ""
	}
	return sanitized
}

// traceShortModel drops the org prefix and the :free suffix. The full id
// already travels in X-Calvoproxy-Model and in the JSON forms; repeating it per
// failed attempt is what pushes the header over its budget.
func traceShortModel(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		model = model[i+1:]
	}
	model = strings.TrimSuffix(model, ":free")
	return traceSanitize(model)
}

// failTrace stamps the outcome and commits the trace headers. Must be called
// BEFORE writeJSONError, which writes the response head.
func failTrace(ctx context.Context, w http.ResponseWriter, outcome string) {
	trace := traceFrom(ctx)
	if trace == nil {
		return
	}
	trace.Outcome = outcome
	setRouteTraceHeadersWithFull(ctx, w.Header(), trace)
}

// traceSanitize keeps header values to a byte set that cannot break framing or
// the field/list separators. Profile and model names come from config, but the
// requested model comes straight off the wire.
func traceSanitize(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '/', r == '+', r == '-':
			return r
		default:
			return '_'
		}
	}, value)
}

// recordTraceFailure annotates one failed attempt. Called from executeAttempt
// rather than from the fallback loop: only executeAttempt sees the upstream's
// raw status, since by the time the loop gets the error cervoretry has already
// remapped it.
func recordTraceFailure(ctx context.Context, attempt modelAttempt, status int, kind, reason string) {
	traceFrom(ctx).recordAttemptFailure(attempt.Model, attempt.AttemptIndex, status, kind, reason)
}

// traceKindFor names the shape of a failure for prev=, from a closed set.
func traceKindFor(attErr *attemptError) string {
	switch {
	case attErr == nil:
		return "err"
	case attErr.SkipModel:
		return "skip"
	case attErr.Timeout:
		return "timeout"
	default:
		return "http"
	}
}
