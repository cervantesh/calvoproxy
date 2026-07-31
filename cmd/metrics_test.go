package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router"
)

func TestProxyMetricsObserve(t *testing.T) {
	m := &proxyMetrics{}
	m.observe(200, 1_000_000)
	m.observe(204, 2_000_000)
	m.observe(404, 500_000)
	m.observe(503, 3_000_000)
	m.observe(302, 100_000)

	if got := m.requestsTotal.Load(); got != 5 {
		t.Errorf("requestsTotal = %d, want 5", got)
	}
	if got := m.status2xx.Load(); got != 2 {
		t.Errorf("2xx = %d, want 2", got)
	}
	if got := m.status4xx.Load(); got != 1 {
		t.Errorf("4xx = %d, want 1", got)
	}
	if got := m.status5xx.Load(); got != 1 {
		t.Errorf("5xx = %d, want 1", got)
	}
	if got := m.statusOther.Load(); got != 1 {
		t.Errorf("other = %d, want 1", got)
	}
	if got := m.latencyCount.Load(); got != 5 {
		t.Errorf("latencyCount = %d, want 5", got)
	}
}

// flushRecorder records whether Flush was called through the wrapper.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestStatusRecorder_ForwardsFlusher(t *testing.T) {
	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := newStatusRecorder(fr)

	// The wrapper must satisfy http.Flusher and forward to the underlying writer,
	// or streaming (SSE) would stop flushing token-by-token.
	flusher, ok := interface{}(rec).(http.Flusher)
	if !ok {
		t.Fatal("statusRecorder must implement http.Flusher")
	}
	flusher.Flush()
	if !fr.flushed {
		t.Fatal("Flush was not forwarded to the underlying writer")
	}
}

func TestStatusRecorder_CapturesStatus(t *testing.T) {
	rec := newStatusRecorder(httptest.NewRecorder())
	rec.WriteHeader(http.StatusTeapot)
	if rec.status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.status, http.StatusTeapot)
	}
	// Implicit 200 when only Write is called.
	rec2 := newStatusRecorder(httptest.NewRecorder())
	rec2.Write([]byte("x"))
	if rec2.status != http.StatusOK {
		t.Errorf("implicit status = %d, want 200", rec2.status)
	}
}

func TestWriteMetrics_RendersRequestSeries(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMetrics(rec, router.NewRouterService().Health())
	body := rec.Body.String()
	for _, want := range []string{
		"calvoproxy_requests_total",
		"calvoproxy_requests_by_status",
		"calvoproxy_request_latency_seconds_sum",
		"calvoproxy_build_info",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
