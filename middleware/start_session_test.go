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

func (d *memoryDriver) Read(id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	value, ok := d.data[id]
	if !ok {
		return "", fmt.Errorf("session [%s] not found", id)
	}
	return value, nil
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
