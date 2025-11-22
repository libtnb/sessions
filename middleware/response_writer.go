package middleware

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
)

// responseWriter is an http.ResponseWriter wrapper that captures the response body and status code.
type responseWriter struct {
	http.ResponseWriter
	body        *bytes.Buffer
	statusCode  int
	written     bool
	passthrough bool
	hijacked    bool
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		body:           bytes.NewBuffer(nil),
		statusCode:     http.StatusOK,
	}
}

func (w *responseWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true

		if code < http.StatusOK {
			// For status codes < 200, switch to passthrough mode
			w.passthrough = true
			w.ResponseWriter.WriteHeader(code)
		}
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

func (w *responseWriter) Flush() {
	if !w.passthrough {
		if w.written {
			w.ResponseWriter.WriteHeader(w.statusCode)
		}
		_, _ = w.ResponseWriter.Write(w.body.Bytes())
	}
	if !w.hijacked {
		if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}
