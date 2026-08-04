package router

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// routerCounters are cheap, lock-free counters for events that only the router
// can see, but that operators alert on. They are exposed through Counters() and
// rendered by the /metrics endpoint.
//
// Why these three: a stream that ends abnormally used to be indistinguishable
// from a clean one (it was even scored a success); admission rejections were
// invisible, so a proxy shedding load looked identical to a healthy one; and
// capability-gated refusals explain 422/503s that aren't upstream failures.
type routerCounters struct {
	streamsCompleted   atomic.Int64
	streamsStalled     atomic.Int64
	streamsUpstreamErr atomic.Int64
	streamsMaxReached  atomic.Int64
	streamsClientGone  atomic.Int64
	admissionRejected  atomic.Int64
	capabilityRefused  atomic.Int64
	// A client named a profile that does not exist. Distinct from a capability
	// refusal: nothing was wrong with the request, the caller asked for something
	// this proxy does not serve — usually a typo or a stale client.
	unknownProfileRejected atomic.Int64
	// /v1/embeddings refused because it would bill real credit — there is no
	// free embedding model upstream. Counted so an operator can see whether
	// anything is actually trying to use it before opting in.
	paidEmbeddingRefused atomic.Int64
	// A streaming attempt abandoned before its first real event. Kept OUT of
	// streamOutcome: it happens before streamCopy runs, so folding it into that
	// enum would make the stream metrics describe something that never streamed.
	streamFirstEventTimeout atomic.Int64
	// Why a fallback chain ended without serving the request, one bucket per
	// chainFailureReason. Before these, a chain that died was a 5xx with no
	// recorded cause: on a live instance 26 of 183 requests failed and nothing
	// said whether the client hung up, a terminal error stopped the chain early,
	// or every model was tried and failed.
	chainFailedCancelled     atomic.Int64
	chainFailedTotalTimeout  atomic.Int64
	chainFailedTerminal      atomic.Int64
	chainFailedExhausted     atomic.Int64
	chainFailedExecutorError atomic.Int64
	// Requests refused BEFORE the chain ran because every planned model was
	// circuit-open or cooling down. Deliberately not a chainFailed bucket: no
	// attempt was made, so folding it in would inflate "the chain failed" with
	// requests the chain never saw. Its neighbours (admissionRejected,
	// capabilityRefused) are counted; this client-visible 503 was not.
	allModelsCooling atomic.Int64

	// Per-model latency, two DIFFERENT quantities that must never be summed
	// together. Guarded by a mutex only for map lookup/insert; the accumulators
	// themselves are atomic, so the hot path never holds the lock while doing
	// arithmetic.
	//
	// firstEventStats: the wait AFTER response headers, which is exactly what
	// PROXY_STREAM_FIRST_BYTE_TIMEOUT compares against. This is the series that
	// says whether that budget is tuned correctly, and nothing else does.
	//
	// firstTokenStats: the whole request → first token, measured from before
	// the upstream call. Measured on a live upstream, these differ by more than
	// rounding: a request with a 1.51s time-to-first-token recorded under 5ms of
	// post-header wait, because OpenRouter held the headers and then flushed its
	// queue keepalives and the first data event together. The post-header wait
	// alone therefore cannot rank models by responsiveness — which is the
	// question an operator actually asks — and the end-to-end number alone
	// cannot say whether the budget is mistuned. Hence both.
	latencyMu       sync.Mutex
	firstEventStats map[string]*firstEventStat
	firstTokenStats map[string]*firstEventStat
}

// observe accumulates one latency sample for a model under the given map.
func (c *routerCounters) observe(into *map[string]*firstEventStat, key string, d time.Duration) {
	c.latencyMu.Lock()
	if *into == nil {
		*into = make(map[string]*firstEventStat)
	}
	stat := (*into)[key]
	if stat == nil {
		stat = &firstEventStat{}
		(*into)[key] = stat
	}
	c.latencyMu.Unlock()
	stat.sumNanos.Add(int64(d))
	stat.count.Add(1)
}

// snapshotLatency renders one latency map, sorted by model so /metrics output
// is stable between scrapes.
// Takes the map by POINTER so the field read happens under the lock: observe
// lazily assigns the map, so reading the field unsynchronized is a data race
// even though every value in it is atomic.
func (c *routerCounters) snapshotLatency(from *map[string]*firstEventStat) []ModelLatency {
	c.latencyMu.Lock()
	out := make([]ModelLatency, 0, len(*from))
	for model, stat := range *from {
		out = append(out, ModelLatency{
			Model:      model,
			SumSeconds: float64(stat.sumNanos.Load()) / float64(time.Second),
			Count:      stat.count.Load(),
		})
	}
	c.latencyMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// firstEventStat accumulates a sum/count pair — the same shape as the
// hand-rolled request_latency metric, which is all a Prometheus-side
// rate(sum)/rate(count) average needs.
type firstEventStat struct {
	sumNanos atomic.Int64
	count    atomic.Int64
}

// chainFailureReason names why a fallback chain ended without serving the
// request. Values are the label set of calvoproxy_chain_failed_total.
type chainFailureReason string

const (
	// chainCancelled: the client hung up. This CANNOT be inferred from the
	// chain's own exit path — see classifyChainFailure.
	chainCancelled chainFailureReason = "cancelled"
	// chainTotalTimeout: the whole-chain wall-clock budget expired.
	chainTotalTimeout chainFailureReason = "total_timeout"
	// chainTerminal: a non-retryable error stopped the chain with models still
	// untried. That "still untried" part is what makes it actionable.
	chainTerminal chainFailureReason = "terminal"
	// chainExhausted: every attempt was made and every one failed.
	chainExhausted chainFailureReason = "exhausted"
	// chainExecutorError: misconfiguration — no attempt executor wired up.
	chainExecutorError chainFailureReason = "executor_error"
)

// ModelLatency is one model's accumulated latency samples, rendered as a
// Prometheus sum/count pair.
type ModelLatency struct {
	// Model is the profile:model breaker key, so this metric's label space is
	// identical to calvoproxy_model_score's and stays bounded by the policy.
	Model      string
	SumSeconds float64
	Count      int64
}

// RouterCounters is the exported snapshot rendered into Prometheus text.
type RouterCounters struct {
	StreamsCompleted        int64
	StreamsStalled          int64
	StreamsUpstreamErr      int64
	StreamsMaxReached       int64
	StreamsClientGone       int64
	AdmissionRejected       int64
	CapabilityRefused       int64
	UnknownProfileRejected  int64
	PaidEmbeddingRefused    int64
	StreamFirstEventTimeout int64

	ChainFailedCancelled     int64
	ChainFailedTotalTimeout  int64
	ChainFailedTerminal      int64
	ChainFailedExhausted     int64
	ChainFailedExecutorError int64
	AllModelsCooling         int64

	// FirstEventLatency is the post-header wait the fail-fast budget acts on;
	// FirstTokenLatency is the end-to-end request → first token. Different
	// quantities over different populations — see routerCounters. Both sorted by
	// model so the /metrics output is stable between scrapes.
	FirstEventLatency []ModelLatency
	FirstTokenLatency []ModelLatency
}

// Counters returns a snapshot of the router's event counters.
func (s *RouterService) Counters() RouterCounters {
	return RouterCounters{
		StreamsCompleted:        s.counters.streamsCompleted.Load(),
		StreamsStalled:          s.counters.streamsStalled.Load(),
		StreamsUpstreamErr:      s.counters.streamsUpstreamErr.Load(),
		StreamsMaxReached:       s.counters.streamsMaxReached.Load(),
		StreamsClientGone:       s.counters.streamsClientGone.Load(),
		AdmissionRejected:       s.counters.admissionRejected.Load(),
		CapabilityRefused:       s.counters.capabilityRefused.Load(),
		UnknownProfileRejected:  s.counters.unknownProfileRejected.Load(),
		PaidEmbeddingRefused:    s.counters.paidEmbeddingRefused.Load(),
		StreamFirstEventTimeout: s.counters.streamFirstEventTimeout.Load(),

		ChainFailedCancelled:     s.counters.chainFailedCancelled.Load(),
		ChainFailedTotalTimeout:  s.counters.chainFailedTotalTimeout.Load(),
		ChainFailedTerminal:      s.counters.chainFailedTerminal.Load(),
		ChainFailedExhausted:     s.counters.chainFailedExhausted.Load(),
		ChainFailedExecutorError: s.counters.chainFailedExecutorError.Load(),
		AllModelsCooling:         s.counters.allModelsCooling.Load(),

		FirstEventLatency: s.counters.snapshotLatency(&s.counters.firstEventStats),
		FirstTokenLatency: s.counters.snapshotLatency(&s.counters.firstTokenStats),
	}
}

// recordChainFailure tallies one failed fallback chain under its reason. Called
// exactly once per failed chain, from the single classification site in
// dispatchChain — the executor stays free of any counter dependency so it can
// be unit-tested standalone.
func (s *RouterService) recordChainFailure(reason chainFailureReason) {
	switch reason {
	case chainCancelled:
		s.counters.chainFailedCancelled.Add(1)
	case chainTotalTimeout:
		s.counters.chainFailedTotalTimeout.Add(1)
	case chainTerminal:
		s.counters.chainFailedTerminal.Add(1)
	case chainExhausted:
		s.counters.chainFailedExhausted.Add(1)
	case chainExecutorError:
		s.counters.chainFailedExecutorError.Add(1)
	}
}

// recordFirstEventLatency records how long an attempt waited for its first
// stream event. Recorded on BOTH outcomes — event arrived and budget expired.
// Timeouts only would make the metric a tail-of-the-tail: every sample would
// equal the budget, and the healthy 0.35-0.70s population that decides whether
// the budget is set correctly would never appear.
//
// Coverage limit, by construction: samples exist only where the first-event
// wait itself runs — streaming attempts, with PROXY_STREAM_FIRST_BYTE_TIMEOUT
// set, that are not last in the chain. Non-streaming attempts and last-chain
// attempts contribute nothing, so Count here is not the model's request count.
func (s *RouterService) recordFirstEventLatency(attempt modelAttempt, d time.Duration) {
	s.counters.observe(&s.counters.firstEventStats, s.breakerKey(attempt), d)
}

// recordFirstTokenLatency records the whole request → first token: the clock
// starts before the upstream call, so unlike recordFirstEventLatency it
// INCLUDES the time spent waiting for response headers. That is where the delay
// actually lived on a live upstream — a 1.51s time-to-first-token recorded as
// under 5ms of post-header wait — so this is the series that ranks models by
// responsiveness.
//
// Recorded ONLY when a real token arrived. An abandoned attempt would otherwise
// contribute "how long we waited before giving up", which is not a
// time-to-first-token at all; averaging the two would drag the number toward
// the budget and make a model look slower the more often it was abandoned. The
// abandonments are counted separately, by
// calvoproxy_stream_first_event_timeout_total.
//
// Same coverage limit as recordFirstEventLatency: sampled only where the
// first-event wait runs — streaming attempts, with
// PROXY_STREAM_FIRST_BYTE_TIMEOUT set, that are not last in the chain. Count is
// therefore not the model's request count.
func (s *RouterService) recordFirstTokenLatency(attempt modelAttempt, d time.Duration) {
	s.counters.observe(&s.counters.firstTokenStats, s.breakerKey(attempt), d)
}

// recordStreamOutcome tallies how a streamed response ended.
func (s *RouterService) recordStreamOutcome(outcome streamOutcome) {
	switch outcome {
	case streamCompleted:
		s.counters.streamsCompleted.Add(1)
	case streamStalled:
		s.counters.streamsStalled.Add(1)
	case streamUpstreamError:
		s.counters.streamsUpstreamErr.Add(1)
	case streamMaxReached:
		s.counters.streamsMaxReached.Add(1)
	case streamClientGone:
		s.counters.streamsClientGone.Add(1)
	}
}
