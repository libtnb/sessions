package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/libtnb/sessions"
)

type memoryDriver struct {
	mu        sync.Mutex
	data      map[string]string
	failWrite bool
}

func newMemoryDriver(failWrite bool) *memoryDriver {
	return &memoryDriver{
		data:      make(map[string]string),
		failWrite: failWrite,
	}
}

func (d *memoryDriver) Close() error {
	return nil
}

func (d *memoryDriver) Destroy(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.data, id)
	return nil
}

func (d *memoryDriver) Gc(int) error {
	return nil
}

func (d *memoryDriver) Touch(id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.data[id]; !ok {
		return false, nil
	}
	return true, nil
}

func (d *memoryDriver) Read(id string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.data[id]
	if !ok {
		return "", false, nil
	}
	return value, true, nil
}

func (d *memoryDriver) Write(id string, data string) error {
	if d.failWrite {
		return fmt.Errorf("write failed")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[id] = data
	return nil
}

func buildManagerWithDriver(t *testing.T, driver *memoryDriver) *sessions.Manager {
	t.Helper()

	manager, err := sessions.NewManager(&sessions.ManagerOptions{
		Key:                  "12345678901234567890123456789012",
		Lifetime:             10,
		GcInterval:           10,
		DisableDefaultDriver: true,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	if err = manager.Extend("mock", driver); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}
	return manager
}

func TestStartSessionSetsCookieWhenSaveSucceeds(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := manager.GetSession(r)
		if err != nil {
			t.Errorf("GetSession failed: %v", err)
			return
		}
		s.Put("k", "v")
		_, _ = w.Write([]byte("ok"))
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if len(rr.Result().Cookies()) == 0 {
		t.Fatal("expected Set-Cookie header on successful save")
	}
}

func TestStartSessionSkipsCookieWhenSaveFails(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(true))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := manager.GetSession(r)
		if err != nil {
			t.Errorf("GetSession failed: %v", err)
			return
		}
		s.Put("k", "v")
		_, _ = w.Write([]byte("ok"))
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rr, req)

	if len(rr.Result().Cookies()) != 0 {
		t.Fatal("did not expect Set-Cookie header when save fails")
	}
}

func TestStartSessionStreamingDoesNotDuplicateBody(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: 1\n\n"))
		w.(http.Flusher).Flush() // SSE handlers flush after each event
		_, _ = w.Write([]byte("event: 2\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("event: 3\n\n"))
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	want := "event: 1\n\nevent: 2\n\nevent: 3\n\n"
	if got := rr.Body.String(); got != want {
		t.Fatalf("streamed body corrupted:\ngot  %q\nwant %q", got, want)
	}
	if len(rr.Result().Cookies()) == 0 {
		t.Fatal("expected session cookie to be sent with the first flush")
	}
}

func TestStartSessionPersistsChangesMadeAfterStreamingStarted(t *testing.T) {
	driver := newMemoryDriver(false)
	manager := buildManagerWithDriver(t, driver)
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("streaming"))
		w.(http.Flusher).Flush() // session is saved here, cookie fixed
		s, err := manager.GetSession(r)
		if err != nil {
			t.Errorf("GetSession failed: %v", err)
			return
		}
		s.Put("late", "change") // after the header went out
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie")
	}

	// A follow-up request must see the late change.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookies[0])
	var got any
	verify := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := manager.GetSession(r)
		if err != nil {
			t.Errorf("GetSession failed: %v", err)
			return
		}
		got = s.Get("late")
	}))
	verify.ServeHTTP(httptest.NewRecorder(), req)

	if got != "change" {
		t.Fatalf("late session change was not persisted, got %v", got)
	}
}

func TestStartSessionCookieSlidesOnEveryResponse(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies1 := rr1.Result().Cookies()
	if len(cookies1) == 0 {
		t.Fatal("expected cookie on first response")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookies1[0])
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	cookies2 := rr2.Result().Cookies()
	if len(cookies2) == 0 {
		t.Fatal("expected cookie to be re-sent so its expiry slides")
	}
	if cookies2[0].Value != cookies1[0].Value {
		t.Fatalf("session ID changed between requests: %s -> %s", cookies1[0].Value, cookies2[0].Value)
	}
	if cookies2[0].MaxAge <= 0 {
		t.Fatalf("expected a positive MaxAge, got %d", cookies2[0].MaxAge)
	}
}

func TestStartSessionReadOnlyVisitorKeepsStableID(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok")) // never writes to the session
	}))

	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies1 := rr1.Result().Cookies()
	if len(cookies1) == 0 {
		t.Fatal("expected cookie on first response")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookies1[0])
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	cookies2 := rr2.Result().Cookies()
	if len(cookies2) > 0 && cookies2[0].Value != cookies1[0].Value {
		t.Fatalf("session ID churned for a read-only visitor: %s -> %s", cookies1[0].Value, cookies2[0].Value)
	}
}

func TestStartSessionSecureFlagOnTLS(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://example.com/", nil))
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}
	if !cookies[0].Secure {
		t.Fatal("expected Secure flag on TLS requests")
	}

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	cookies = rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}
	if cookies[0].Secure {
		t.Fatal("did not expect Secure flag on plain HTTP requests")
	}
}

func TestStartSessionWithConfigCookieCallback(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSessionWithConfig(manager, Config{
		Driver: "mock",
		Cookie: func(c *http.Cookie) {
			c.Path = "/app"
			c.SameSite = http.SameSiteStrictMode
			c.Secure = true
		},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected cookie")
	}
	c := cookies[0]
	if c.Path != "/app" || c.SameSite != http.SameSiteStrictMode || !c.Secure {
		t.Fatalf("cookie callback not applied: %+v", c)
	}
}

func TestStartSessionPreservesHandlerStatusCode(t *testing.T) {
	manager := buildManagerWithDriver(t, newMemoryDriver(false))
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not here"))
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if rr.Body.String() != "not here" {
		t.Fatalf("body = %q", rr.Body.String())
	}
}

func TestStartSessionDoesNotReissueCookieForDestroyedSession(t *testing.T) {
	d := newMemoryDriver(false)
	manager := buildManagerWithDriver(t, d)

	// Request 1: establish a session
	seed := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := manager.GetSession(r)
		s.Put("user", "someone")
	}))
	rr1 := httptest.NewRecorder()
	seed.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/", nil))
	cookie := rr1.Result().Cookies()[0]

	// Request 2: while it is in flight, a concurrent logout destroys the
	// session (simulated inside the handler, after Start loaded it).
	handler := StartSession(manager, "mock")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := manager.GetSession(r)
		other, err := manager.BuildSession(sessions.CookieName, "mock")
		if err != nil {
			t.Errorf("BuildSession failed: %v", err)
			return
		}
		other.SetID(s.GetID())
		other.Start()
		if err = other.Invalidate(); err != nil {
			t.Errorf("Invalidate failed: %v", err)
		}
		if err = other.Save(); err != nil {
			t.Errorf("Save failed: %v", err)
		}
		manager.ReleaseSession(other)
		_, _ = w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req)

	// The stale request must not re-issue the destroyed session's cookie —
	// that would overwrite the browser's regenerated ID.
	for _, c := range rr2.Result().Cookies() {
		if c.Value == cookie.Value {
			t.Fatal("destroyed session cookie was re-issued by a stale request")
		}
	}

	// And the destroyed ID must not have been written back to the store.
	d.mu.Lock()
	_, resurrected := d.data[cookie.Value]
	d.mu.Unlock()
	if resurrected {
		t.Fatal("destroyed session was resurrected in the store")
	}
}
