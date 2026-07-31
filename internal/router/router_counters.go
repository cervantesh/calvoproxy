package router

import "sync/atomic"

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
}

// RouterCounters is the exported snapshot rendered into Prometheus text.
type RouterCounters struct {
	StreamsCompleted   int64
	StreamsStalled     int64
	StreamsUpstreamErr int64
	StreamsMaxReached  int64
	StreamsClientGone  int64
	AdmissionRejected  int64
	CapabilityRefused  int64
}

// Counters returns a snapshot of the router's event counters.
func (s *RouterService) Counters() RouterCounters {
	return RouterCounters{
		StreamsCompleted:   s.counters.streamsCompleted.Load(),
		StreamsStalled:     s.counters.streamsStalled.Load(),
		StreamsUpstreamErr: s.counters.streamsUpstreamErr.Load(),
		StreamsMaxReached:  s.counters.streamsMaxReached.Load(),
		StreamsClientGone:  s.counters.streamsClientGone.Load(),
		AdmissionRejected:  s.counters.admissionRejected.Load(),
		CapabilityRefused:  s.counters.capabilityRefused.Load(),
	}
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
