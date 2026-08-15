package router

import (
	"testing"
	"time"
)

func TestParseProfileTimeoutsRejectsOutOfRangeValues(t *testing.T) {
	timeouts, ok := parseProfileTimeouts([]byte(`{"Timeouts":{"titlegen":15,"bad":0,"huge":100000,"negative":-5,"":9}}`))
	if !ok {
		t.Fatal("expected the Timeouts section to parse")
	}
	if len(timeouts) != 1 {
		t.Fatalf("only the valid entry should survive, got %#v", timeouts)
	}
	if timeouts["titlegen"] != 15*time.Second {
		t.Fatalf("expected 15s for titlegen, got %s", timeouts["titlegen"])
	}
}

func TestParseProfileTimeoutsAcceptsFractionalSeconds(t *testing.T) {
	timeouts, _ := parseProfileTimeouts([]byte(`{"Timeouts":{"titlegen":2.5}}`))
	if timeouts["titlegen"] != 2500*time.Millisecond {
		t.Fatalf("expected 2.5s, got %s", timeouts["titlegen"])
	}
}

// An override exists to make a profile LESS patient. Letting it widen the
// deadline would promise something the transport's fixed ResponseHeaderTimeout
// cannot honour.
func TestAttemptTimeoutForProfileOnlyShrinks(t *testing.T) {
	svc := &RouterService{profileTimeouts: profileTimeouts{
		"titlegen": 15 * time.Second,
		"greedy":   10 * time.Minute,
	}}

	if got := svc.attemptTimeoutForProfile("titlegen", 90*time.Second); got != 15*time.Second {
		t.Fatalf("a smaller override should apply, got %s", got)
	}
	if got := svc.attemptTimeoutForProfile("greedy", 90*time.Second); got != 90*time.Second {
		t.Fatalf("a larger override must not widen the deadline, got %s", got)
	}
	if got := svc.attemptTimeoutForProfile("coding", 90*time.Second); got != 90*time.Second {
		t.Fatalf("an unlisted profile keeps the caller's value, got %s", got)
	}
}

func TestAttemptTimeoutForProfileIsSafeWithoutConfiguration(t *testing.T) {
	var nilService *RouterService
	if got := nilService.attemptTimeoutForProfile("titlegen", 42*time.Second); got != 42*time.Second {
		t.Fatalf("nil service must pass the value through, got %s", got)
	}
	empty := &RouterService{}
	if got := empty.attemptTimeoutForProfile("titlegen", 42*time.Second); got != 42*time.Second {
		t.Fatalf("unconfigured service must pass the value through, got %s", got)
	}
}

// The shipped policy must stay loadable by the code that reads it, or the
// per-profile deadlines silently do nothing in production.
func TestShippedPolicyTimeoutsLoad(t *testing.T) {
	timeouts, ok := parseProfileTimeouts(defaultModelConfigJSON)
	if !ok || len(timeouts) == 0 {
		t.Fatalf("embedded default policy should define a Timeouts section, got %#v ok=%v", timeouts, ok)
	}
	// The two per-turn profiles are the whole reason this exists: they must not
	// inherit a deadline meant for a coding turn.
	for _, profile := range []string{"titlegen", "skillsearch"} {
		if timeouts[profile] <= 0 || timeouts[profile] > 30*time.Second {
			t.Fatalf("%s should carry a short deadline, got %s", profile, timeouts[profile])
		}
	}
}
