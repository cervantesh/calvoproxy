package router

import "testing"

// Invariant 2: recent() is newest-first and honours the limit. A dashboard that
// shows the oldest 20 of 200 decisions is worse than useless — it looks live.
func TestTraceRing_RecentIsNewestFirstAndBounded(t *testing.T) {
	ring := newTraceRing(10)
	for _, id := range []string{"a", "b", "c", "d"} {
		ring.add(&routeTrace{ID: id, Profile: "coding"})
	}

	got := ring.recent(2)
	if len(got) != 2 {
		t.Fatalf("recent(2) returned %d entries, want 2", len(got))
	}
	if got[0].ID != "d" || got[1].ID != "c" {
		t.Errorf("recent = %s,%s; want d,c (newest first)", got[0].ID, got[1].ID)
	}
}

// A limit larger than the contents returns everything, not padding.
func TestTraceRing_RecentReturnsAllWhenLimitExceedsContents(t *testing.T) {
	ring := newTraceRing(10)
	ring.add(&routeTrace{ID: "solo", Profile: "coding"})

	if got := ring.recent(50); len(got) != 1 {
		t.Errorf("recent(50) over one entry returned %d", len(got))
	}
}

// Invariant 3: an empty or nil ring is empty, never a panic. The dashboard polls
// from the moment the proxy starts, which is exactly when the ring is empty.
func TestTraceRing_RecentHandlesEmptyAndNil(t *testing.T) {
	if got := newTraceRing(5).recent(10); len(got) != 0 {
		t.Errorf("fresh ring returned %d entries", len(got))
	}
	var nilRing *traceRing
	if got := nilRing.recent(10); got != nil {
		t.Errorf("nil ring returned %v, want nil", got)
	}
}

// Wrapping keeps newest-first: once the ring rolls, the oldest entries are gone
// and the order must not flip at the seam.
func TestTraceRing_RecentSurvivesWrapAround(t *testing.T) {
	ring := newTraceRing(3)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		ring.add(&routeTrace{ID: id, Profile: "coding"})
	}

	got := ring.recent(3)
	if len(got) != 3 {
		t.Fatalf("recent(3) returned %d", len(got))
	}
	for i, want := range []string{"5", "4", "3"} {
		if got[i].ID != want {
			t.Errorf("recent[%d] = %s, want %s", i, got[i].ID, want)
		}
	}
}
