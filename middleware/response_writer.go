package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
)

// responseWriter passes the response straight through to the underlying
// writer. beforeHeader runs once, right before the first byte (header or
// body) is handed over — the last moment at which headers can still change.
type responseWriter struct {
	http.ResponseWriter
	beforeHeader func()
	once         sync.Once
}

// Unwrap returns the underlying ResponseWriter so that
// http.NewResponseController can reach deadline setters and similar
// optional interfaces.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// commit runs beforeHeader unless it already ran, and reports whether this
// call was the one that ran it.
func (w *responseWriter) commit() bool {
	ran := false
	w.once.Do(func() {
		w.beforeHeader()
		ran = true
	})
	return ran
}

func (w *responseWriter) WriteHeader(code int) {
	// 1xx responses are informational and the final header is still to
	// come; 101 is the exception, nothing follows it.
	if code < 100 || code > 199 || code == http.StatusSwitchingProtocols {
		w.commit()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	w.commit()
	return w.ResponseWriter.Write(data)
}

// ReadFrom keeps the underlying writer's io.ReaderFrom fast path (sendfile)
// reachable for io.Copy and http.ServeContent.
func (w *responseWriter) ReadFrom(src io.Reader) (int64, error) {
	w.commit()
	return io.Copy(w.ResponseWriter, src)
}

func (w *responseWriter) Flush() {
	_ = w.FlushError()
}

func (w *responseWriter) FlushError() error {
	w.commit()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack does not commit: the handler takes over the connection, so no
// header of ours will be sent.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
