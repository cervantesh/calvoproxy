package router

import (
	"sort"
	"sync"
	"time"
)

// QuotaBucketKey identifies one independently enforced upstream quota. ModelOrPool
// deliberately supports both per-model limits (for example Groq) and a shared
// pool (for example OpenRouter's free models).
//
// Scope is the upstream enforcement scope when it is known, such as "organization",
// "project", or "account". A provider must not reuse a key across scopes unless it
// has evidence that the quota is genuinely shared.
type QuotaBucketKey struct {
	Provider    string
	Scope       string
	ModelOrPool string
	Dimension   QuotaDimension
	Window      QuotaWindow
}

// QuotaDimension is deliberately small, while the string type keeps the ledger
// forward-compatible with provider-specific dimensions.
type QuotaDimension string

const (
	QuotaDimensionRequests QuotaDimension = "requests"
	QuotaDimensionTokens   QuotaDimension = "tokens"
)

// QuotaWindow names the period over which a quota is enforced.
type QuotaWindow string

const (
	QuotaWindowMinute QuotaWindow = "minute"
	QuotaWindowHour   QuotaWindow = "hour"
	QuotaWindowDay    QuotaWindow = "day"
)

// QuotaSource says where an observation came from. ProviderHeader is preferred
// over an estimate because account limits can differ from published defaults.
type QuotaSource string

const (
	QuotaSourceProviderHeader QuotaSource = "provider_header"
	QuotaSourceProviderBody   QuotaSource = "provider_body"
	QuotaSourceConfig         QuotaSource = "config"
	QuotaSourceLocalEstimate  QuotaSource = "local_estimate"
)

// QuotaConfidence describes how safe it is to make routing decisions from an
// observation. The ledger retains this for callers and does not invent a value.
type QuotaConfidence string

const (
	QuotaConfidenceAuthoritative QuotaConfidence = "authoritative"
	QuotaConfidenceEstimated     QuotaConfidence = "estimated"
)

// QuotaObservation is the provider's view of a bucket. Remaining is always
// normalised to [0, Limit], preventing a malformed header from creating a
// negative available balance. ResetAt is optional; a zero value means that the
// observation has no known expiry.
type QuotaObservation struct {
	Limit      int64
	Remaining  int64
	ResetAt    time.Time
	Source     QuotaSource
	Confidence QuotaConfidence
	ObservedAt time.Time
}

// QuotaSnapshot combines a provider observation with proxy-local in-flight
// reservations. Available is the only balance a scheduler should spend.
type QuotaSnapshot struct {
	QuotaObservation
	Key       QuotaBucketKey
	Reserved  int64
	Available int64
}

// QuotaReservation is an atomic unit to reserve, release, or reconcile. A
// single request commonly has one requests reservation and one tokens
// reservation, which must succeed or fail together.
type QuotaReservation struct {
	Key  QuotaBucketKey
	Cost int64
}

// QuotaEstimate is the routing-time estimate for a request. Providers may map
// either or both values into QuotaReservation values, depending on the headers
// they expose.
type QuotaEstimate struct {
	Requests       int64
	Tokens         int64
	InputTokens    int64
	OutputTokens   int64
	OutputExplicit bool
}

// QuotaSettlement settles one dimension held by a QuotaTicket. Actual is used
// only when no provider observation is supplied: with an authoritative
// remaining header, that header already includes the completed request and wins
// over estimates.
type QuotaSettlement struct {
	Key         QuotaBucketKey
	Actual      int64
	Observation *QuotaObservation
}

// QuotaTicket is an opaque, ledger-owned reservation handle. A ticket can be
// released or reconciled exactly once. This ownership is essential: amount-
// based cleanup cannot distinguish a duplicate cleanup for request A from a
// valid reservation belonging to request B.
type QuotaTicket struct {
	ledger *QuotaLedger
	id     uint64
}

func (t QuotaTicket) Valid() bool { return t.ledger != nil && t.id != 0 }

// Reservations returns copies of the dimensions owned by an active ticket.
// It is stable even when a reset replaces a bucket with the same logical key.
func (l *QuotaLedger) Reservations(ticket QuotaTicket) []QuotaReservation {
	if l == nil || ticket.ledger != l || ticket.id == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.tickets[ticket.id]
	if !ok {
		return nil
	}
	out := make([]QuotaReservation, 0, len(state.holds))
	for key, hold := range state.holds {
		out = append(out, QuotaReservation{Key: key, Cost: hold.cost})
	}
	sort.Slice(out, func(i, j int) bool { return quotaKeyString(out[i].Key) < quotaKeyString(out[j].Key) })
	return out
}

type quotaBucket struct {
	observation QuotaObservation
	reserved    int64
}

type quotaHold struct {
	key    QuotaBucketKey
	cost   int64
	bucket *quotaBucket
}

type quotaTicketState struct {
	holds map[QuotaBucketKey]quotaHold
}

// QuotaLedger is an in-memory, concurrency-safe view of upstream quotas. It
// intentionally has no provider-specific header parsing: adapters should turn
// native provider responses into QuotaObservation before reaching this layer.
type QuotaLedger struct {
	mu           sync.Mutex
	buckets      map[QuotaBucketKey]*quotaBucket
	tickets      map[uint64]quotaTicketState
	nextTicketID uint64
}

func NewQuotaLedger() *QuotaLedger {
	return &QuotaLedger{
		buckets: make(map[QuotaBucketKey]*quotaBucket),
		tickets: make(map[uint64]quotaTicketState),
	}
}

// ReservationsForEstimate maps a request/token estimate to the supplied
// buckets, omitting zero costs. It is a convenience for provider adapters and
// preserves atomicity when passed to ReserveAll.
func ReservationsForEstimate(requestBucket, tokenBucket QuotaBucketKey, estimate QuotaEstimate) []QuotaReservation {
	reservations := make([]QuotaReservation, 0, 2)
	if estimate.Requests > 0 {
		reservations = append(reservations, QuotaReservation{Key: requestBucket, Cost: estimate.Requests})
	}
	if estimate.Tokens > 0 {
		reservations = append(reservations, QuotaReservation{Key: tokenBucket, Cost: estimate.Tokens})
	}
	return reservations
}

// Observe records a provider observation. Expired observations are discarded
// rather than retained as a permanent zero quota.
func (l *QuotaLedger) Observe(key QuotaBucketKey, observation QuotaObservation, now time.Time) bool {
	if !validQuotaKey(key) || observation.Limit < 0 || observation.Remaining < 0 {
		return false
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if !observation.ResetAt.IsZero() && !observation.ResetAt.After(now) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.purgeExpiredLocked(now)
		delete(l.buckets, key)
		return false
	}
	if observation.Remaining > observation.Limit {
		observation.Remaining = observation.Limit
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	bucket := l.buckets[key]
	if bucket == nil {
		bucket = &quotaBucket{}
		l.buckets[key] = bucket
	}
	bucket.observation = observation
	return true
}

// Snapshot returns the current bucket state. Buckets whose known reset has
// passed are removed and reported as absent.
func (l *QuotaLedger) Snapshot(key QuotaBucketKey, now time.Time) (QuotaSnapshot, bool) {
	if l == nil {
		return QuotaSnapshot{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	bucket := l.buckets[key]
	if bucket == nil {
		return QuotaSnapshot{}, false
	}
	// A provider may report a minute bucket without a reset timestamp. Such a
	// value cannot remain authoritative past its observed minute: keeping it
	// would turn one old 429 into a permanent all-cooling state.
	if key.Window == QuotaWindowMinute && bucket.observation.ResetAt.IsZero() &&
		!bucket.observation.ObservedAt.IsZero() && now.Truncate(time.Minute).After(bucket.observation.ObservedAt.Truncate(time.Minute)) {
		delete(l.buckets, key)
		return QuotaSnapshot{}, false
	}
	available := bucket.observation.Remaining - bucket.reserved
	if available < 0 {
		available = 0
	}
	return QuotaSnapshot{
		QuotaObservation: bucket.observation,
		Key:              key,
		Reserved:         bucket.reserved,
		Available:        available,
	}, true
}

// ReserveAll atomically reserves every known bucket in reservations and returns
// the unique ticket that owns those reservations. Unknown buckets impose no
// local hard limit until an adapter observes or configures them, but remain on
// the ticket so a response observation can establish them during reconciliation.
// A negative cost is invalid; a zero cost is ignored.
func (l *QuotaLedger) ReserveAll(reservations []QuotaReservation, now time.Time) (QuotaTicket, bool) {
	if l == nil {
		return QuotaTicket{}, false
	}
	amounts, ok := aggregateReservations(reservations)
	if !ok {
		return QuotaTicket{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	for key, cost := range amounts {
		if bucket := l.buckets[key]; bucket != nil && bucket.observation.Remaining-bucket.reserved < cost {
			return QuotaTicket{}, false
		}
	}
	holds := make(map[QuotaBucketKey]quotaHold, len(amounts))
	for key, cost := range amounts {
		bucket := l.buckets[key]
		if bucket != nil {
			bucket.reserved += cost
		}
		holds[key] = quotaHold{key: key, cost: cost, bucket: bucket}
	}
	ticketID := l.newTicketIDLocked()
	l.tickets[ticketID] = quotaTicketState{holds: holds}
	return QuotaTicket{ledger: l, id: ticketID}, true
}

// Reserve is the single-bucket form of ReserveAll.
func (l *QuotaLedger) Reserve(key QuotaBucketKey, cost int64, now time.Time) (QuotaTicket, bool) {
	return l.ReserveAll([]QuotaReservation{{Key: key, Cost: cost}}, now)
}

// Release removes all in-flight reservations owned by ticket. It succeeds
// exactly once. Duplicate, forged, foreign-ledger, and zero tickets are no-ops
// that return false.
func (l *QuotaLedger) Release(ticket QuotaTicket, now time.Time) bool {
	if l == nil || ticket.ledger != l || ticket.id == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	state, ok := l.consumeTicketLocked(ticket)
	if !ok {
		return false
	}
	l.releaseHoldsLocked(state)
	return true
}

// Reconcile atomically settles a ticket exactly once. For dimensions with an
// observation, provider remaining is authoritative. Otherwise Actual is
// committed after releasing the reservation. A reserved dimension omitted from
// settlements is charged its estimated cost conservatively.
func (l *QuotaLedger) Reconcile(ticket QuotaTicket, settlements []QuotaSettlement, now time.Time) bool {
	if l == nil || ticket.ledger != l || ticket.id == 0 {
		return false
	}
	byKey := make(map[QuotaBucketKey]QuotaSettlement, len(settlements))
	for _, settlement := range settlements {
		if !validQuotaKey(settlement.Key) || settlement.Actual < 0 {
			return false
		}
		if _, duplicate := byKey[settlement.Key]; duplicate {
			return false
		}
		if observation := settlement.Observation; observation != nil &&
			(observation.Limit < 0 || observation.Remaining < 0) {
			return false
		}
		byKey[settlement.Key] = settlement
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	state, exists := l.tickets[ticket.id]
	if !exists {
		return false
	}
	for key := range byKey {
		if _, held := state.holds[key]; !held {
			return false
		}
	}
	delete(l.tickets, ticket.id)
	for key, hold := range state.holds {
		if hold.bucket != nil {
			hold.bucket.reserved -= minInt64(hold.bucket.reserved, hold.cost)
		}
		settlement, supplied := byKey[key]
		if supplied && settlement.Observation != nil {
			observation := normaliseQuotaObservation(*settlement.Observation, now)
			if !observation.ResetAt.IsZero() && !observation.ResetAt.After(now) {
				delete(l.buckets, key)
				continue
			}
			bucket := l.buckets[key]
			if bucket == nil {
				bucket = &quotaBucket{}
				l.buckets[key] = bucket
			}
			bucket.observation = observation
			continue
		}
		actual := hold.cost
		if supplied {
			actual = settlement.Actual
		}
		if hold.bucket != nil {
			hold.bucket.observation.Remaining -= minInt64(hold.bucket.observation.Remaining, actual)
		}
	}
	return true
}

func (l *QuotaLedger) consumeTicketLocked(ticket QuotaTicket) (quotaTicketState, bool) {
	state, ok := l.tickets[ticket.id]
	if !ok {
		return quotaTicketState{}, false
	}
	delete(l.tickets, ticket.id)
	return state, true
}

func (l *QuotaLedger) releaseHoldsLocked(state quotaTicketState) {
	for _, hold := range state.holds {
		if hold.bucket != nil {
			hold.bucket.reserved -= minInt64(hold.bucket.reserved, hold.cost)
		}
	}
}

func (l *QuotaLedger) newTicketIDLocked() uint64 {
	for {
		l.nextTicketID++
		if l.nextTicketID == 0 {
			continue
		}
		if _, exists := l.tickets[l.nextTicketID]; !exists {
			return l.nextTicketID
		}
	}
}

func (l *QuotaLedger) purgeExpiredLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if !bucket.observation.ResetAt.IsZero() && !bucket.observation.ResetAt.After(now) {
			delete(l.buckets, key)
		}
	}
}

func aggregateReservations(reservations []QuotaReservation) (map[QuotaBucketKey]int64, bool) {
	amounts := make(map[QuotaBucketKey]int64, len(reservations))
	for _, reservation := range reservations {
		if !validQuotaKey(reservation.Key) || reservation.Cost < 0 {
			return nil, false
		}
		if reservation.Cost == 0 {
			continue
		}
		amounts[reservation.Key] += reservation.Cost
		if amounts[reservation.Key] < 0 { // int64 overflow
			return nil, false
		}
	}
	return amounts, true
}

func validQuotaKey(key QuotaBucketKey) bool {
	return key.Provider != "" && key.Scope != "" && key.ModelOrPool != "" && key.Dimension != "" && key.Window != ""
}

func normaliseQuotaObservation(observation QuotaObservation, now time.Time) QuotaObservation {
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if observation.Remaining > observation.Limit {
		observation.Remaining = observation.Limit
	}
	return observation
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

// Keys returns the currently non-expired keys in a deterministic order. It is
// primarily useful for health reporting and tests; callers receive copies only.
func (l *QuotaLedger) Keys(now time.Time) []QuotaBucketKey {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.purgeExpiredLocked(now)
	keys := make([]QuotaBucketKey, 0, len(l.buckets))
	for key := range l.buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return quotaKeyString(keys[i]) < quotaKeyString(keys[j])
	})
	return keys
}

func quotaKeyString(key QuotaBucketKey) string {
	return key.Provider + "\x00" + key.Scope + "\x00" + key.ModelOrPool + "\x00" + string(key.Dimension) + "\x00" + string(key.Window)
}
