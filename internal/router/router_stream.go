package router

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// isEventStream reports whether an upstream response is a Server-Sent Events
// stream (OpenAI/OpenRouter emit `Content-Type: text/event-stream` when the
// request set `stream: true`). Such responses must be piped through with
// flushing, not buffered.
func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// streamCopy pipes an upstream body to the client, flushing after every chunk so
// tokens reach the caller as they arrive (real streaming) instead of being
// buffered until the end.
//
// It replaces the old whole-request client timeout (which silently cut long but
// live streams) with two bounds that do not penalise a healthy long stream:
//   - idle: max gap allowed BETWEEN chunks. Reset on every non-empty read; if it
//     lapses the stream is aborted (a stalled upstream can't pin the connection).
//   - max: an absolute backstop on total stream lifetime (0 disables it) so even
//     a pathological keepalive that never idles cannot pin a goroutine forever.
//
// Reads run in a dedicated goroutine so the select can honour the timers and the
// request context; on any exit the body is closed, which unblocks that read.
func streamCopy(ctx context.Context, w http.ResponseWriter, body io.ReadCloser, idle, max time.Duration) {
	// Self-contained: close the body on EVERY exit path (idle/max/ctx/write-error
	// close it explicitly to unblock the reader immediately; this defer covers the
	// clean-EOF path too). A double close on an http body is harmless.
	defer func() { _ = body.Close() }()
	flusher, _ := w.(http.Flusher)

	// Single reader goroutine → chunks/err. `done` lets it unblock on any exit
	// path; closing `body` unblocks the pending Read itself.
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk)
	done := make(chan struct{})
	defer close(done)

	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case chunks <- chunk{data: cp}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case chunks <- chunk{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	idleTimer := time.NewTimer(idle)
	defer idleTimer.Stop()

	var maxC <-chan time.Time
	if max > 0 {
		maxTimer := time.NewTimer(max)
		defer maxTimer.Stop()
		maxC = maxTimer.C
	}

	for {
		select {
		case c := <-chunks:
			if len(c.data) > 0 {
				if _, werr := w.Write(c.data); werr != nil {
					_ = body.Close()
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(idle)
			}
			if c.err != nil {
				return // normal EOF or upstream error: stream ended
			}
		case <-idleTimer.C:
			_ = body.Close() // stalled upstream: unblock the reader and stop
			return
		case <-maxC:
			_ = body.Close() // absolute lifetime backstop
			return
		case <-ctx.Done():
			_ = body.Close() // client disconnect / cancellation
			return
		}
	}
}
