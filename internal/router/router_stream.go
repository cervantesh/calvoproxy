package router

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// isEventStream reports whether an upstream response is a Server-Sent Events
// stream (OpenAI/OpenRouter emit `Content-Type: text/event-stream` when the
// request set `stream: true`). Such responses must be piped through with
// flushing, not buffered.
func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// streamCopy pipes an upstream body to the client, flushing after every chunk
// so tokens reach the caller as they arrive (real streaming) instead of being
// buffered until the end. Stops on write error or a cancelled context.
func streamCopy(ctx context.Context, w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}
