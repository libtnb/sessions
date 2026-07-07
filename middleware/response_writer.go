package middleware

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
)

// responseWriter buffers the response so the session middleware can still
// set headers (the session cookie) after the handler has run. beforeHeader,
// when set, is invoked once, right before the header is sent to the
// underlying writer — this is the last moment at which headers can change.
type responseWriter struct {
	http.ResponseWriter
	body         *bytes.Buffer
	statusCode   int
	written      bool // WriteHeader was called by the handler
	headerSent   bool // header was flushed to the underlying writer
	passthrough  bool
	hijacked     bool
	beforeHeader func()
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		body:           bytes.NewBuffer(nil),
		statusCode:     http.StatusOK,
	}
}

// Unwrap returns the underlying ResponseWriter so that
// http.NewResponseController can reach deadline setters and similar
// optional interfaces.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *responseWriter) WriteHeader(code int) {
	if w.written || w.headerSent {
		return
	}
	w.statusCode = code
	w.written = true

	if code < http.StatusOK {
		// For status codes < 200, switch to passthrough mode
		w.passthrough = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if w.passthrough {
		return w.ResponseWriter.Write(data)
	}
	return w.body.Write(data)
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.passthrough && w.body.Len() > 0 {
		w.Flush()
	}
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.hijacked = true
		return hijacker.Hijack()
	}
	panic("ResponseWriter doesn't implement http.Hijacker")
}

// Flush sends the buffered header and body to the underlying writer. It is
// safe to call multiple times (e.g. by streaming handlers): the header goes
// out once and the buffer is drained after each flush, so nothing is ever
// written twice.
func (w *responseWriter) Flush() {
	if w.hijacked {
		return
	}
	if !w.passthrough {
		if !w.headerSent {
			if w.beforeHeader != nil {
				w.beforeHeader()
			}
			w.headerSent = true
			w.ResponseWriter.WriteHeader(w.statusCode)
		}
		if w.body.Len() > 0 {
			_, _ = w.ResponseWriter.Write(w.body.Bytes())
			w.body.Reset()
		}
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
