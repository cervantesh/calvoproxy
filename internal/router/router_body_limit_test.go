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
