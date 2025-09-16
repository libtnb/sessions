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
	body       *bytes.Buffer
	statusCode int
	written    bool
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
	}
}

func (w *responseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	panic("ResponseWriter doesn't implement http.Hijacker")
}

func (w *responseWriter) Flush() {
	w.flush()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseWriter) flush() {
	if w.written {
		w.ResponseWriter.WriteHeader(w.statusCode)
	}
	_, _ = w.ResponseWriter.Write(w.body.Bytes())
}
