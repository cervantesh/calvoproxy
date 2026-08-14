package main

import (
	"net/http"
	"sync/atomic"
)

// proxyMetrics holds cheap, lock-free counters for the request-level signals an
// operator actually alerts on (rate, errors, latency) — complementing the
// per-model breaker/score gauges already in ProxyHealth. Histograms are
// intentionally deferred; a sum+count gives a usable average without the weight.
type proxyMetrics struct {
	requestsTotal atomic.Int64
	status2xx     atomic.Int64
	status4xx     atomic.Int64
	status5xx     atomic.Int64
	statusOther   atomic.Int64
	latencyNanos  atomic.Int64 // sum of handler durations
	latencyCount  atomic.Int64
}

var metrics = &proxyMetrics{}

func (m *proxyMetrics) observe(status int, durNanos int64) {
	m.requestsTotal.Add(1)
	switch {
	case status >= 200 && status < 300:
		m.status2xx.Add(1)
	case status >= 400 && status < 500:
		m.status4xx.Add(1)
	case status >= 500:
		m.status5xx.Add(1)
	default:
		m.statusOther.Add(1)
	}
	m.latencyNanos.Add(durNanos)
	m.latencyCount.Add(1)
}

// statusRecorder wraps a ResponseWriter to capture the final status code. It
// MUST forward http.Flusher (and expose Unwrap) so streamed (SSE) responses keep
// flushing token-by-token — a naive wrapper would silently break streaming.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return // superfluous WriteHeader; keep the first status and don't re-send
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true // implicit 200 on first write without WriteHeader
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer's flusher so streaming still flushes.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer for http.ResponseController and any other
// optional-interface probing.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
