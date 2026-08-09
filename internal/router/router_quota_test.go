package router

import (
	"sync"
	"testing"
	"time"
)

func quotaTestKey(dimension QuotaDimension) QuotaBucketKey {
	return QuotaBucketKey{
		Provider:    "groq",
		Scope:       "organization",
		ModelOrPool: "openai/gpt-oss-120b",
		Dimension:   dimension,
		Window:      QuotaWindowMinute,
	}
}

func quotaTestObservation(limit, remaining int64, reset time.Time) QuotaObservation {
	return QuotaObservation{
		Limit:      limit,
		Remaining:  remaining,
		ResetAt:    reset,
		Source:     QuotaSourceProviderHeader,
		Confidence: QuotaConfidenceAuthoritative,
	}
}

func TestQuotaLedgerReserveEstimateIsAtomicAcrossRequestsAndTokens(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	requests := quotaTestKey(QuotaDimensionRequests)
	tokens := quotaTestKey(QuotaDimensionTokens)
	reset := now.Add(time.Minute)
	if !ledger.Observe(requests, quotaTestObservation(10, 1, reset), now) ||
		!ledger.Observe(tokens, quotaTestObservation(1_000, 100, reset), now) {
		t.Fatal("could not set up quota buckets")
	}

	reservations := ReservationsForEstimate(requests, tokens, QuotaEstimate{Requests: 1, Tokens: 101})
	if _, ok := ledger.ReserveAll(reservations, now); ok {
		t.Fatal("reservation should fail when one dimension lacks capacity")
	}
	if snapshot, ok := ledger.Snapshot(requests, now); !ok || snapshot.Reserved != 0 || snapshot.Available != 1 {
		t.Fatalf("request quota must remain untouched after atomic failure: %+v ok=%v", snapshot, ok)
	}
	if snapshot, ok := ledger.Snapshot(tokens, now); !ok || snapshot.Reserved != 0 || snapshot.Available != 100 {
		t.Fatalf("token quota must remain untouched after atomic failure: %+v ok=%v", snapshot, ok)
	}

	reservations = ReservationsForEstimate(requests, tokens, QuotaEstimate{Requests: 1, Tokens: 100})
	if _, ok := ledger.ReserveAll(reservations, now); !ok {
		t.Fatal("reservation should succeed when all dimensions have capacity")
	}
	if snapshot, _ := ledger.Snapshot(requests, now); snapshot.Available != 0 || snapshot.Reserved != 1 {
		t.Fatalf("unexpected request snapshot: %+v", snapshot)
	}
	if snapshot, _ := ledger.Snapshot(tokens, now); snapshot.Available != 0 || snapshot.Reserved != 100 {
		t.Fatalf("unexpected token snapshot: %+v", snapshot)
	}
}

func TestQuotaLedgerReconcileUsesProviderHeadersAndNeverDropsBelowZero(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionTokens)
	if !ledger.Observe(key, quotaTestObservation(100, 100, now.Add(time.Minute)), now) {
		t.Fatal("could not observe quota")
	}
	ticket, ok := ledger.Reserve(key, 25, now)
	if !ok {
		t.Fatal("could not reserve quota")
	}

	// The provider's remaining value already includes this completed request.
	if !ledger.Reconcile(ticket, []QuotaSettlement{{
		Key:    key,
		Actual: 20,
		Observation: &QuotaObservation{
			Limit:      100,
			Remaining:  80,
			ResetAt:    now.Add(time.Minute),
			Source:     QuotaSourceProviderHeader,
			Confidence: QuotaConfidenceAuthoritative,
		},
	}}, now) {
		t.Fatal("reconcile with provider header failed")
	}
	if snapshot, _ := ledger.Snapshot(key, now); snapshot.Reserved != 0 || snapshot.Remaining != 80 || snapshot.Available != 80 {
		t.Fatalf("authoritative observation should win: %+v", snapshot)
	}

	ticket, ok = ledger.Reserve(key, 80, now)
	if !ok {
		t.Fatal("could not reserve remaining quota")
	}
	if !ledger.Reconcile(ticket, []QuotaSettlement{{Key: key, Actual: 999}}, now) {
		t.Fatal("reconcile without header failed")
	}
	if snapshot, _ := ledger.Snapshot(key, now); snapshot.Remaining != 0 || snapshot.Available != 0 || snapshot.Reserved != 0 {
		t.Fatalf("remaining must clamp at zero: %+v", snapshot)
	}
}

func TestQuotaLedgerExpiredBucketsAreIgnoredAndRemoved(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionRequests)
	if !ledger.Observe(key, quotaTestObservation(10, 0, now.Add(time.Second)), now) {
		t.Fatal("could not observe quota")
	}
	if _, ok := ledger.Reserve(key, 1, now); ok {
		t.Fatal("known exhausted quota must deny reservation")
	}
	later := now.Add(2 * time.Second)
	if _, ok := ledger.Snapshot(key, later); ok {
		t.Fatal("expired bucket should be removed")
	}
	if _, ok := ledger.Reserve(key, 1, later); !ok {
		t.Fatal("expired observation must not permanently deny a new window")
	}
	if keys := ledger.Keys(later); len(keys) != 0 {
		t.Fatalf("an unknown reservation must not recreate an expired bucket: %+v", keys)
	}
}

func TestQuotaLedgerConcurrentReservationsCannotOverspend(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionRequests)
	if !ledger.Observe(key, quotaTestObservation(40, 40, now.Add(time.Minute)), now) {
		t.Fatal("could not observe quota")
	}

	const contenders = 200
	var wg sync.WaitGroup
	results := make(chan bool, contenders)
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok := ledger.Reserve(key, 1, now)
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for success := range results {
		if success {
			successes++
		}
	}
	if successes != 40 {
		t.Fatalf("got %d successful reservations, want exactly 40", successes)
	}
	snapshot, ok := ledger.Snapshot(key, now)
	if !ok || snapshot.Reserved != 40 || snapshot.Available != 0 || snapshot.Remaining < 0 {
		t.Fatalf("concurrent reserve overspent quota: %+v ok=%v", snapshot, ok)
	}
}

func TestQuotaLedgerTicketDoubleReleaseCannotReleaseAnotherOwner(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionRequests)
	if !ledger.Observe(key, quotaTestObservation(2, 2, now.Add(time.Minute)), now) {
		t.Fatal("could not observe quota")
	}
	ticketA, ok := ledger.Reserve(key, 1, now)
	if !ok {
		t.Fatal("could not reserve A")
	}
	ticketB, ok := ledger.Reserve(key, 1, now)
	if !ok {
		t.Fatal("could not reserve B")
	}

	if !ledger.Release(ticketA, now) {
		t.Fatal("first release of A should succeed")
	}
	if ledger.Release(ticketA, now) {
		t.Fatal("second release of A must be rejected")
	}
	snapshot, _ := ledger.Snapshot(key, now)
	if snapshot.Reserved != 1 || snapshot.Available != 1 {
		t.Fatalf("duplicate release of A affected B: %+v", snapshot)
	}
	if !ledger.Reconcile(ticketB, []QuotaSettlement{{Key: key, Actual: 1}}, now) {
		t.Fatal("B should remain independently settleable")
	}
	snapshot, _ = ledger.Snapshot(key, now)
	if snapshot.Reserved != 0 || snapshot.Remaining != 1 || snapshot.Available != 1 {
		t.Fatalf("unexpected snapshot after settling B: %+v", snapshot)
	}
}

func TestQuotaLedgerRejectsUnknownAndForeignTickets(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	other := NewQuotaLedger()
	unknown := QuotaTicket{ledger: ledger, id: 999}
	if ledger.Release(QuotaTicket{}, now) || ledger.Release(unknown, now) {
		t.Fatal("zero and unknown tickets must be rejected")
	}
	if ledger.Reconcile(unknown, nil, now) {
		t.Fatal("unknown ticket reconciliation must be rejected")
	}
	foreign, ok := other.Reserve(quotaTestKey(QuotaDimensionRequests), 1, now)
	if !ok {
		t.Fatal("could not create foreign ticket")
	}
	if ledger.Release(foreign, now) || ledger.Reconcile(foreign, nil, now) {
		t.Fatal("ticket from another ledger must be rejected")
	}
}

func TestQuotaLedgerTicketSettlementIsExactlyOnceUnderConcurrency(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionRequests)
	if !ledger.Observe(key, quotaTestObservation(10, 10, now.Add(time.Minute)), now) {
		t.Fatal("could not observe quota")
	}
	ticket, ok := ledger.Reserve(key, 1, now)
	if !ok {
		t.Fatal("could not reserve quota")
	}

	const contenders = 50
	results := make(chan bool, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- ledger.Release(ticket, now)
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for success := range results {
		if success {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("ticket settled %d times, want exactly once", successes)
	}
	if snapshot, _ := ledger.Snapshot(key, now); snapshot.Reserved != 0 {
		t.Fatalf("reservation remained after settlement: %+v", snapshot)
	}
}

func TestQuotaLedgerObservationClampsMalformedRemainingAndUsesObservedAt(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	ledger := NewQuotaLedger()
	key := quotaTestKey(QuotaDimensionRequests)
	if !ledger.Observe(key, QuotaObservation{
		Limit:      10,
		Remaining:  99,
		ResetAt:    now.Add(time.Minute),
		Source:     QuotaSourceConfig,
		Confidence: QuotaConfidenceEstimated,
	}, now) {
		t.Fatal("could not observe quota")
	}
	snapshot, ok := ledger.Snapshot(key, now)
	if !ok || snapshot.Remaining != 10 || !snapshot.ObservedAt.Equal(now) {
		t.Fatalf("observation was not normalised: %+v ok=%v", snapshot, ok)
	}
	if ledger.Observe(key, quotaTestObservation(-1, 0, now.Add(time.Minute)), now) {
		t.Fatal("negative limit must be rejected")
	}
}
