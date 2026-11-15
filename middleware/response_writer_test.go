package middleware

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// plainWriter is a ResponseWriter with no optional interfaces. It is safe
// for concurrent use and only counts the body.
type plainWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	size   int64
}

func newPlainWriter() *plainWriter {
	return &plainWriter{header: make(http.Header)}
}

func (w *plainWriter) Header() http.Header {
	return w.header
}

func (w *plainWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}

func (w *plainWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.size += int64(len(data))
	return len(data), nil
}

// readerFromWriter records whether the body arrived through ReadFrom.
type readerFromWriter struct {
	*httptest.ResponseRecorder
	readFrom       int
	cookieOnHeader bool
}

func (w *readerFromWriter) ReadFrom(src io.Reader) (int64, error) {
	w.readFrom++
	w.cookieOnHeader = w.Header().Get("Set-Cookie") != ""
	return io.Copy(w.ResponseRecorder, src)
}

// serve runs handler behind the session middleware on a real HTTP server.
// done receives a value each time the middleware has returned.
func serve(t *testing.T, d *memoryDriver, handler http.HandlerFunc) (srv *httptest.Server, done chan struct{}) {
	t.Helper()

	manager := buildManagerWithDriver(t, d)
	done = make(chan struct{}, 8)
	wrapped := StartSession(manager, "mock")(handler)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		wrapped.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

// waitFor blocks until ch delivers, failing the test when it takes too long.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestResponseWriterStreamsBodyWhileHandlerRuns(t *testing.T) {
	const size = 4 << 20
	chunk := make([]byte, 32<<10)

	received := make(chan struct{})
	srv, _ := serve(t, newMemoryDriver(false), func(w http.ResponseWriter, r *http.Request) {
		for written := 0; written < size; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				t.Errorf("Write failed: %v", err)
				return
			}
		}
		// No Flush and no return: the client must have the body anyway.
		select {
		case <-received:
		case <-time.After(10 * time.Second):
			t.Error("body did not reach the client while the handler was running")
		}
	})

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	sessionCookie(t, resp.Header)

	// The server may hold back the tail of the last chunk until the handler returns.
	if _, err = io.CopyN(io.Discard, resp.Body, size-int64(len(chunk))); err != nil {
		t.Fatalf("reading the streamed body failed: %v", err)
	}
	close(received)

	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the rest of the body failed: %v", err)
	}
}

func TestResponseWriterDoesNotBufferBody(t *testing.T) {
	const size = 64 << 20
	chunk := make([]byte, 32<<10)

	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for written := 0; written < size; written += len(chunk) {
			_, _ = w.Write(chunk)
		}
	}))
	w := newPlainWriter()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	handler.ServeHTTP(w, req)
	runtime.ReadMemStats(&after)

	if w.size != size {
		t.Fatalf("underlying writer got %d bytes, want %d", w.size, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes while writing a %d byte body", allocated, size)
	}
}

func TestResponseWriterFlushReachesClient(t *testing.T) {
	flushes := map[string]func(w http.ResponseWriter) error{
		"Flusher": func(w http.ResponseWriter) error {
			w.(http.Flusher).Flush()
			return nil
		},
		"ResponseController": func(w http.ResponseWriter) error {
			return http.NewResponseController(w).Flush()
		},
	}
	for name, flush := range flushes {
		t.Run(name, func(t *testing.T) {
			received := make(chan struct{})
			srv, _ := serve(t, newMemoryDriver(false), func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "event: 1\n\n")
				if err := flush(w); err != nil {
					t.Errorf("Flush failed: %v", err)
				}
				select {
				case <-received:
				case <-time.After(10 * time.Second):
					t.Error("flushed event did not reach the client while the handler was running")
				}
				_, _ = io.WriteString(w, "event: 2\n\n")
			})

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			sessionCookie(t, resp.Header)

			first := make([]byte, len("event: 1\n\n"))
			if _, err = io.ReadFull(resp.Body, first); err != nil {
				t.Fatalf("reading the first event failed: %v", err)
			}
			close(received)

			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the rest failed: %v", err)
			}
			if got := string(first) + string(rest); got != "event: 1\n\nevent: 2\n\n" {
				t.Fatalf("body = %q", got)
			}
		})
	}
}

func TestResponseWriterWithoutOptionalInterfaces(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.Flush(); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Flush error = %v, want http.ErrNotSupported", err)
		}
		if _, _, err := rc.Hijack(); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Hijack error = %v, want http.ErrNotSupported", err)
		}
		w.(http.Flusher).Flush()
		if _, _, err := w.(http.Hijacker).Hijack(); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Hijack error = %v, want http.ErrNotSupported", err)
		}
		// ReadFrom falls back to plain writes.
		if n, err := io.Copy(w, io.LimitReader(strings.NewReader("body"), 4)); n != 4 || err != nil {
			t.Errorf("Copy = %d, %v", n, err)
		}
	}))

	w := newPlainWriter()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.size != 4 {
		t.Fatalf("underlying writer got %d bytes, want 4", w.size)
	}
	sessionCookie(t, w.header)
}

func TestResponseWriterHijack(t *testing.T) {
	d := newMemoryDriver(false)
	srv, done := serve(t, d, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = rw.Flush()
	})

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "hijacked" {
		t.Fatalf("body = %q, err = %v", body, err)
	}
	// The handler wrote the response itself; the middleware adds nothing.
	if len(resp.Cookies()) != 0 {
		t.Fatalf("unexpected cookies on a hijacked response: %v", resp.Cookies())
	}

	// The session is still saved once the handler is done.
	waitFor(t, done, "the middleware to return")
	if writes, _ := d.ops(); writes != 1 {
		t.Fatalf("session stored %d times, want 1", writes)
	}
}

func TestResponseWriterUpgrade(t *testing.T) {
	d := newMemoryDriver(false)
	upgraded := make(chan struct{})
	srv, done := serve(t, d, func(w http.ResponseWriter, r *http.Request) {
		// The way WebSocket libraries accept a connection.
		w.Header().Set("Upgrade", "test")
		w.Header().Set("Connection", "Upgrade")
		w.WriteHeader(http.StatusSwitchingProtocols)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if writes, _ := d.ops(); writes != 1 {
			t.Errorf("session stored %d times before the upgrade, want 1", writes)
		}
		close(upgraded)

		line, err := rw.ReadString('\n')
		if err != nil {
			t.Errorf("reading from the hijacked connection failed: %v", err)
			return
		}
		_, _ = rw.WriteString("echo " + line)
		_ = rw.Flush()
	})

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "test")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	sessionCookie(t, resp.Header)
	waitFor(t, upgraded, "the upgrade")

	conn, ok := resp.Body.(io.ReadWriter)
	if !ok {
		t.Fatalf("upgraded body %T is not writable", resp.Body)
	}
	if _, err = io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("writing to the upgraded connection failed: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "echo ping\n" {
		t.Fatalf("got %q, err = %v", line, err)
	}

	waitFor(t, done, "the middleware to return")
	if writes, touches := d.ops(); writes != 1 || touches != 0 {
		t.Fatalf("writes=%d touches=%d after the upgrade, want 1 and 0", writes, touches)
	}
}

func TestResponseWriterReadFromUsesUnderlyingReaderFrom(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// LimitReader has no WriteTo, so io.Copy goes for the writer's ReadFrom.
		if n, err := io.Copy(w, io.LimitReader(strings.NewReader("zero copy"), 9)); n != 9 || err != nil {
			t.Errorf("Copy = %d, %v", n, err)
		}
	}))

	w := &readerFromWriter{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.readFrom != 1 {
		t.Fatalf("underlying ReadFrom called %d times, want 1", w.readFrom)
	}
	if !w.cookieOnHeader {
		t.Fatal("session cookie was not set before the body was handed over")
	}
	if w.Body.String() != "zero copy" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestResponseWriterServeContent(t *testing.T) {
	content := strings.Repeat("0123456789abcdef", 64<<10) // 1 MiB
	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	srv, _ := serve(t, newMemoryDriver(false), func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(path)
		if err != nil {
			t.Errorf("Open failed: %v", err)
			return
		}
		defer func() { _ = f.Close() }()
		http.ServeContent(w, r, "file.bin", time.Time{}, f)
	})

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(body) != content {
		t.Fatalf("got %d bytes, want the %d byte file", len(body), len(content))
	}
	if resp.ContentLength != int64(len(content)) {
		t.Fatalf("Content-Length = %d, want %d", resp.ContentLength, len(content))
	}
	sessionCookie(t, resp.Header)

	// Range requests keep working.
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Range", "bytes=16-31")
	partial, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() { _ = partial.Body.Close() }()
	body, err = io.ReadAll(partial.Body)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if partial.StatusCode != http.StatusPartialContent || string(body) != content[16:32] {
		t.Fatalf("status = %d, body = %q", partial.StatusCode, body)
	}
}

func TestResponseWriterUnwrap(t *testing.T) {
	srv, _ := serve(t, newMemoryDriver(false), func(w http.ResponseWriter, r *http.Request) {
		// Only the server's own writer knows about deadlines.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline failed: %v", err)
		}
	})

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	_ = resp.Body.Close()
}

func TestResponseWriterRepeatedWriteHeader(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not here"))
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the first one, 404", rr.Code)
	}
	if rr.Body.String() != "not here" {
		t.Fatalf("body = %q", rr.Body.String())
	}
	sessionCookie(t, rr.Result().Header)
}

func TestResponseWriterInformationalResponse(t *testing.T) {
	d := newMemoryDriver(false)
	manager := buildManagerWithDriver(t, d)
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := manager.GetSession(r)
		if err != nil {
			t.Errorf("GetSession failed: %v", err)
			return
		}
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		if writes, _ := d.ops(); writes != 0 {
			t.Errorf("session stored %d times before the final header, want 0", writes)
		}

		s.Put("k", "v")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	var hints []textproto.MIMEHeader
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			if code != http.StatusEarlyHints {
				return fmt.Errorf("unexpected informational status %d", code)
			}
			hints = append(hints, header)
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if len(hints) != 1 {
		t.Fatalf("got %d informational responses, want 1", len(hints))
	}
	if hints[0].Get("Link") == "" || hints[0].Get("Set-Cookie") != "" {
		t.Fatalf("informational response header = %v, want Link and no Set-Cookie", hints[0])
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := sessionValue(t, manager, sessionCookie(t, resp.Header), "k"); got != "v" {
		t.Fatalf("change made after the informational response was lost, got %v", got)
	}
}

func TestResponseWriterConcurrentFirstWrites(t *testing.T) {
	const writers = 32

	d := newMemoryDriver(false)
	manager := buildManagerWithDriver(t, d)
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = w.Write([]byte("x"))
			}()
		}
		close(start)
		wg.Wait()
	}))

	w := newPlainWriter()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.size != writers {
		t.Fatalf("underlying writer got %d bytes, want %d", w.size, writers)
	}
	sessionCookie(t, w.header)
	if writes, touches := d.ops(); writes != 1 || touches != 0 {
		t.Fatalf("writes=%d touches=%d, want the session stored exactly once", writes, touches)
	}
}
