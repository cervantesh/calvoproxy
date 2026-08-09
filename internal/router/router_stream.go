package router

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// isEventStream reports whether an upstream response is a Server-Sent Events
// stream (OpenAI/OpenRouter emit `Content-Type: text/event-stream` when the
// request set `stream: true`). Such responses must be piped through with
// flushing, not buffered.
func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// streamOutcome says how a streamed response ended, so the caller can score the
// model honestly. Before this existed, a stream was recorded as a success the
// moment its 200 headers arrived — a stalled or truncated stream still counted as
// healthy, which is exactly the failure mode that matters most for long agent
// streams.
type streamOutcome int

const (
	// streamCompleted: upstream signalled a clean end of body (EOF).
	streamCompleted streamOutcome = iota
	// streamStalled: no bytes for the idle window — an upstream fault.
	streamStalled
	// streamUpstreamError: a non-EOF read error mid-stream — an upstream fault.
	streamUpstreamError
	// streamMaxReached: our own absolute lifetime backstop fired. Local policy,
	// not necessarily the model's fault.
	streamMaxReached
	// streamClientGone: client disconnected/cancelled, or writing to it failed.
	// Never the model's fault.
	streamClientGone
)

// upstreamFault reports whether the outcome indicates the MODEL/upstream misbehaved
// (as opposed to the client going away or our own backstop firing).
func (o streamOutcome) upstreamFault() bool {
	return o == streamStalled || o == streamUpstreamError
}

func (o streamOutcome) String() string {
	switch o {
	case streamCompleted:
		return "completed"
	case streamStalled:
		return "stalled (idle timeout)"
	case streamUpstreamError:
		return "upstream read error"
	case streamMaxReached:
		return "max duration reached"
	case streamClientGone:
		return "client disconnected"
	default:
		return "unknown"
	}
}

// streamCopy pipes an upstream body to the client, flushing after every chunk so
// tokens reach the caller as they arrive (real streaming) instead of being
// buffered until the end. It returns HOW the stream ended (see streamOutcome).
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
func streamCopy(ctx context.Context, w http.ResponseWriter, body io.ReadCloser, idle, max time.Duration) streamOutcome {
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

	// The outcome is decided by WHICH select case wins — not by inspecting
	// ctx.Err() afterwards, which races with a concurrent client disconnect.
	for {
		select {
		case c := <-chunks:
			if len(c.data) > 0 {
				if _, werr := w.Write(c.data); werr != nil {
					_ = body.Close()
					return streamClientGone // can't write to the client anymore
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
				if errors.Is(c.err, io.EOF) {
					return streamCompleted // clean end of stream
				}
				// A cancelled request tears down the upstream read too, so this
				// error can be the CLIENT leaving rather than the model failing.
				// Both cases race in the select; check the context before blaming
				// the model, or a user hitting Ctrl-C would open its circuit.
				if ctx.Err() != nil {
					return streamClientGone
				}
				return streamUpstreamError // upstream died mid-stream
			}
		case <-idleTimer.C:
			// The idle timer and a just-arrived chunk can fire together; prefer
			// real data so a healthy stream is never mislabelled as stalled.
			select {
			case c := <-chunks:
				if len(c.data) > 0 {
					if _, werr := w.Write(c.data); werr != nil {
						_ = body.Close()
						return streamClientGone
					}
					if flusher != nil {
						flusher.Flush()
					}
					idleTimer.Reset(idle)
					continue
				}
				if c.err != nil {
					if errors.Is(c.err, io.EOF) {
						return streamCompleted
					}
					if ctx.Err() != nil {
						return streamClientGone
					}
					return streamUpstreamError
				}
				idleTimer.Reset(idle)
				continue
			default:
			}
			_ = body.Close() // stalled upstream: unblock the reader and stop
			return streamStalled
		case <-maxC:
			_ = body.Close() // absolute lifetime backstop (our policy, not theirs)
			return streamMaxReached
		case <-ctx.Done():
			_ = body.Close() // client disconnect / cancellation
			return streamClientGone
		}
	}
}

// --- fail-fast on the first stream event -------------------------------------

// firstEventReader stitches the bytes consumed while waiting for the first SSE
// event back in front of the still-buffered reader, so nothing is lost between
// the pre-commit wait and streamCopy. Close goes to the original body.
type firstEventReader struct {
	io.Reader
	closer io.Closer
}

func (f firstEventReader) Close() error { return f.closer.Close() }

// awaitFirstStreamEvent waits for the first SSE event that actually carries
// payload, so the caller can abandon a queued model BEFORE committing headers
// to the client.
//
// Keepalive comments do NOT count as progress. OpenRouter emits
// `: OPENROUTER PROCESSING` precisely while a request sits queued upstream —
// the exact condition this wait exists to detect — so treating any byte as
// arrival would defeat the mechanism in the only case that matters.
//
// Returns a reader positioned as if nothing had been consumed, whether a real
// event arrived, and whether the wait timed out (as opposed to the stream
// simply ending).
type firstEventResult struct {
	consumed []byte
	ok       bool
	reader   *bufio.Reader
}

type pendingFirstEventReader struct {
	body   io.ReadCloser
	result <-chan firstEventResult
	once   sync.Once
	reader io.Reader
}

func (r *pendingFirstEventReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		result := <-r.result
		r.reader = io.MultiReader(bytes.NewReader(result.consumed), result.reader)
	})
	return r.reader.Read(p)
}

func (r *pendingFirstEventReader) Close() error {
	return r.body.Close()
}

func awaitFirstStreamEvent(body io.ReadCloser, timeout time.Duration) (io.ReadCloser, bool, bool) {
	br := bufio.NewReader(body)
	ch := make(chan firstEventResult, 1) // buffered: an abandoned reader must not block forever
	go func() {
		var seen bytes.Buffer
		for {
			line, err := br.ReadBytes('\n')
			seen.Write(line)
			if err != nil {
				ch <- firstEventResult{consumed: seen.Bytes(), reader: br}
				return
			}
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 || trimmed[0] == ':' {
				continue // blank separator or keepalive comment: not progress
			}
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				ch <- firstEventResult{consumed: seen.Bytes(), ok: true, reader: br}
				return
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return firstEventReader{Reader: io.MultiReader(bytes.NewReader(r.consumed), r.reader), closer: body}, r.ok, false
	case <-timer.C:
		// Keep ownership of the single in-flight reader. If the caller elects to
		// continue because no fallback quota can be claimed, the wrapper waits
		// for that read and replays everything consumed. If it abandons, closing
		// the wrapper unblocks the read and the buffered send lets it exit.
		return &pendingFirstEventReader{body: body, result: ch}, false, true
	}
}
