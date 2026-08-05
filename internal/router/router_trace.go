package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
}

type traceCtxKey struct{}

// newRouteTrace mints a trace with the id the client is handed as
// X-Calvoproxy-Decision-Id.
func newRouteTrace(profile string) *routeTrace {
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
)

// header renders the short form: the stable, always-on contract. Only codes and
// closed enumerations go in here — never upstream error text, which is what
// /decisions/{id} is for (spec §6).
func (t *routeTrace) header() string {
	if t == nil {
		return ""
	}
	fields := []string{
		traceVersion,
		"p=" + traceSanitize(t.Profile),
		// Always emitted, so "not compressed" stays distinguishable from "this
		// field does not exist" once P3 lands.
		"cmp=" + traceNoCompress,
	}
	return strings.Join(fields, traceFieldSep)
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
