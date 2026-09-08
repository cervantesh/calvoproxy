package router

import (
	"os"
	"testing"
)

// The request-body cap is a transport guard, not a context limit. It must stay
// well clear of legitimate traffic so that oversized conversations reach
// filterContextFit, which knows the models' real windows and answers 422 with
// actionable text, instead of dying at MaxBytesReader with a bare 413.
//
// Observed 2026-09-07: a session carrying accumulated base64 tool-result
// imagery crossed the old 10 MiB default while using 17% of its TOKEN budget.
// Bytes and tokens are different axes; only the token axis has a ceiling worth
// enforcing here, and it is enforced elsewhere.
func TestRequestBodyCapLeavesRoomForMultimodalConversations(t *testing.T) {
	t.Setenv("PROXY_MAX_BODY_BYTES", "")

	got := maxRequestBodyBytes()

	const tenMiB = 10 << 20
	if got <= tenMiB {
		t.Fatalf("default body cap is %d bytes (<= the old %d): a multimodal "+
			"conversation is rejected with 413 at the handler door, before "+
			"filterContextFit can decide on the models' real windows", got, tenMiB)
	}
	// Still bounded: the cap has to stop a hostile client, so it must not be
	// removed altogether while chasing the 413.
	const oneGiB = 1 << 30
	if got >= oneGiB {
		t.Fatalf("default body cap is %d bytes (>= %d): that is no longer a "+
			"guard against an abusive client", got, oneGiB)
	}
}

// The env override has to keep working, including for operators who want the
// old conservative value back.
func TestRequestBodyCapHonoursEnvOverride(t *testing.T) {
	t.Setenv("PROXY_MAX_BODY_BYTES", "12345")
	if got := maxRequestBodyBytes(); got != 12345 {
		t.Fatalf("PROXY_MAX_BODY_BYTES=12345 gave %d; the override is ignored", got)
	}

	// Garbage must fall back to the default rather than to zero, which would
	// reject every request.
	t.Setenv("PROXY_MAX_BODY_BYTES", "no-soy-un-numero")
	if got := maxRequestBodyBytes(); got <= 0 {
		t.Fatalf("invalid PROXY_MAX_BODY_BYTES gave %d; a non-positive cap "+
			"rejects every request body", got)
	}
	_ = os.Unsetenv("PROXY_MAX_BODY_BYTES")
}

// Regression for the gap that PR #120 shipped with and review caught: raising
// the TRANSPORT cap alone changes nothing, because a second, independent limit
// sits in front of it.
//
// authorizeOperationalRoute runs at router.go:195/239/279, before dispatchChain
// at 254/295 and therefore before filterContextFit. The compiled policy carries
// its own deny-oversized-body threshold, so a body between the old 10 MiB and
// the new cap passed MaxBytesReader and was then refused by policy — the 413
// simply became a 403, and the context-aware 422 was still unreachable.
//
// This asserts the property directly instead of the constant: a body the
// transport now admits must survive the policy too.
func TestPolicyAdmitsBodiesTheTransportCapNowAllows(t *testing.T) {
	t.Setenv("PROXY_MAX_BODY_BYTES", "")

	const oldTransportCap = 10 << 20
	if maxRequestBodyBytes() <= oldTransportCap {
		t.Skip("transport cap is back at or below the old default; nothing to admit")
	}

	// Comfortably past the old ceiling, comfortably inside the new one.
	body := bodyOfSize(oldTransportCap + (1 << 20))

	decision, ok, recorder := decideForBody(t, body)
	if !ok {
		t.Fatalf("policy refused a %d-byte body that the transport cap admits "+
			"(status %d, reason %q). The transport default and the policy "+
			"threshold have drifted apart, so raising one accomplishes nothing.",
			len(body), recorder.Code, decision.Reason)
	}
}

// The two limits must not drift again: a body one byte under the policy
// threshold has to be admitted, and the policy threshold has to be at least the
// transport cap, or the transport cap is decorative.
func TestPolicyThresholdIsNotBelowTheTransportCap(t *testing.T) {
	t.Setenv("PROXY_MAX_BODY_BYTES", "")

	if policyBodyLimit < int(maxRequestBodyBytes()) {
		t.Fatalf("policy threshold %d is below the transport cap %d: every "+
			"request between them is refused by policy, so the transport cap "+
			"never decides anything", policyBodyLimit, maxRequestBodyBytes())
	}
}
