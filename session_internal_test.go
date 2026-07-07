package sessions

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/libtnb/sessions/driver"
)

type memoryDriver struct {
	mu        sync.Mutex
	data      map[string]string
	writes    int
	touches   int
	closes    int
	failWrite bool
	failTouch bool // Touch returns a non-not-found error
	failRead  bool // Read returns a non-not-found error
}

func newMemoryDriver() *memoryDriver {
	return &memoryDriver{
		data: make(map[string]string),
	}
}

func (d *memoryDriver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
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
	if d.failTouch {
		return false, fmt.Errorf("store unavailable")
	}
	if _, ok := d.data[id]; !ok {
		return false, nil
	}
	d.touches++
	return true, nil
}

func (d *memoryDriver) Read(id string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRead {
		return "", false, fmt.Errorf("store unavailable")
	}
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
	d.writes++
	return nil
}

func testManagerWithDriver(t *testing.T, d *memoryDriver) *Manager {
	t.Helper()

	m, err := NewManager(&ManagerOptions{
		Key:                  "12345678901234567890123456789012",
		Lifetime:             10,
		GcInterval:           10,
		DisableDefaultDriver: true,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	m.drivers["mock"] = d
	return m
}

func TestSessionSaveMergesConcurrentWrites(t *testing.T) {
	driver := newMemoryDriver()
	manager := testManagerWithDriver(t, driver)

	seed, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	seed.Start()
	seed.Put("seed", 1)
	if err = seed.Save(); err != nil {
		t.Fatalf("seed Save failed: %v", err)
	}
	sessionID := seed.GetID()
	manager.ReleaseSession(seed)

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			s, err := manager.BuildSession(CookieName, "mock")
			if err != nil {
				t.Errorf("BuildSession failed: %v", err)
				return
			}
			s.SetID(sessionID)
			s.Start()
			s.Put(fmt.Sprintf("k%d", i), i)
			if err = s.Save(); err != nil {
				t.Errorf("Save failed: %v", err)
			}
			manager.ReleaseSession(s)
		}(i)
	}
	wg.Wait()

	result, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	result.SetID(sessionID)
	result.Start()
	if got := result.Get("seed"); got != 1 {
		t.Fatalf("seed key lost, got=%v", got)
	}
	for i := range workers {
		key := fmt.Sprintf("k%d", i)
		if got := result.Get(key); got != i {
			t.Fatalf("missing or wrong value for %s: got=%v want=%d", key, got, i)
		}
	}
	manager.ReleaseSession(result)
}

func TestSessionSaveTouchesWhenNotDirty(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	// Create and save a session with some data
	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	s1.Put("key", "value")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	sessionID := s1.GetID()
	manager.ReleaseSession(s1)

	// Record touches after initial save
	d.mu.Lock()
	touchesBefore := d.touches
	d.mu.Unlock()

	// Open the same session but don't modify it
	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(sessionID)
	s2.Start()

	if got := s2.Get("key"); got != "value" {
		t.Fatalf("expected 'value', got %v", got)
	}

	// Save without modifications — should call Touch to refresh timestamp
	if err = s2.Save(); err != nil {
		t.Fatalf("Save (non-dirty) failed: %v", err)
	}
	manager.ReleaseSession(s2)

	d.mu.Lock()
	touchesAfter := d.touches
	d.mu.Unlock()

	if touchesAfter <= touchesBefore {
		t.Fatal("expected driver.Touch to be called for non-dirty session to refresh timestamp")
	}

	// Verify the session data is still intact
	s3, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s3.SetID(sessionID)
	s3.Start()
	if got := s3.Get("key"); got != "value" {
		t.Fatalf("expected 'value' after non-dirty save, got %v", got)
	}
	manager.ReleaseSession(s3)
}

func TestSessionRejectsUnsafeID(t *testing.T) {
	manager := testManagerWithDriver(t, newMemoryDriver())
	session, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	defer manager.ReleaseSession(session)

	session.SetID("../12345678901234567890123456789")
	if got := session.GetID(); got == "../12345678901234567890123456789" {
		t.Fatal("expected unsafe session id to be rejected")
	}
	if len(session.GetID()) != 32 {
		t.Fatalf("expected regenerated session id length 32, got %q", session.GetID())
	}
}

func TestSessionStartRegeneratesUnknownClientID(t *testing.T) {
	driver := newMemoryDriver()
	manager := testManagerWithDriver(t, driver)

	session, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	unknownID := "12345678901234567890123456789012"
	session.SetID(unknownID)
	session.Start()
	if got := session.GetID(); got == unknownID {
		t.Fatal("expected unknown client-provided session id to be regenerated")
	}
	session.Put("key", "value")
	if err = session.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	newID := session.GetID()
	manager.ReleaseSession(session)

	driver.mu.Lock()
	_, oldExists := driver.data[unknownID]
	_, newExists := driver.data[newID]
	driver.mu.Unlock()
	if oldExists {
		t.Fatal("unknown client-provided session id was persisted")
	}
	if !newExists {
		t.Fatal("regenerated session id was not persisted")
	}
}

func TestManagerSessionLocksAreCleanedUp(t *testing.T) {
	manager, err := NewManager(&ManagerOptions{
		Key:                  "12345678901234567890123456789012",
		Lifetime:             10,
		GcInterval:           10,
		DisableDefaultDriver: true,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	sessionID := "12345678901234567890123456789012"
	manager.LockSession(sessionID)

	done := make(chan struct{})
	go func() {
		manager.LockSession(sessionID)
		manager.UnlockSession(sessionID)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	manager.UnlockSession(sessionID)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiting locker goroutine timed out")
	}

	deadline := time.Now().Add(time.Second)
	for {
		manager.sessionLocksMu.Lock()
		count := len(manager.sessionLocks)
		manager.sessionLocksMu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session lock map not cleaned up, count=%d", count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionFlashLifecycle(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	// Request 1: flash a message
	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	s1.Flash("msg", "hello")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	id := s1.GetID()
	manager.ReleaseSession(s1)

	// Request 2: the flash value is visible
	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(id)
	s2.Start()
	if got := s2.Get("msg"); got != "hello" {
		t.Fatalf("flash value not visible on next request, got %v", got)
	}
	if err = s2.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	manager.ReleaseSession(s2)

	// Request 3: the flash value is gone
	s3, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s3.SetID(id)
	s3.Start()
	if got := s3.Get("msg"); got != nil {
		t.Fatalf("flash value should be aged out, got %v", got)
	}
	manager.ReleaseSession(s3)
}

func TestSessionKeepExtendsFlash(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	s1.Flash("msg", "hello")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	id := s1.GetID()
	manager.ReleaseSession(s1)

	// Request 2: keep the flash for one more request
	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(id)
	s2.Start()
	s2.Keep("msg")
	if err = s2.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	manager.ReleaseSession(s2)

	// Request 3: still visible thanks to Keep
	s3, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s3.SetID(id)
	s3.Start()
	if got := s3.Get("msg"); got != "hello" {
		t.Fatalf("kept flash value should survive, got %v", got)
	}
	manager.ReleaseSession(s3)
}

func TestSessionPollutedFlashDataDoesNotPanic(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	s, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	defer manager.ReleaseSession(s)
	s.Start()

	// Overwrite the internal flash bookkeeping with foreign types
	s.Put("_flash.new", "junk")
	s.Put("_flash.old", 123)
	s.Flash("msg", "hello")
	s.Keep("other")
	s.Reflash()
	if err = s.Save(); err != nil {
		t.Fatalf("Save with polluted flash data failed: %v", err)
	}
}

func TestSessionNonDirtySavePersistsFreshSession(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	// A read-only visitor: no Put before Save
	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	id := s1.GetID()
	manager.ReleaseSession(s1)

	d.mu.Lock()
	_, persisted := d.data[id]
	d.mu.Unlock()
	if !persisted {
		t.Fatal("fresh session should be persisted so the ID stays stable")
	}

	// The next request keeps the same ID
	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(id)
	s2.Start()
	if s2.GetID() != id {
		t.Fatalf("session ID churned: %s -> %s", id, s2.GetID())
	}
	manager.ReleaseSession(s2)
}

func TestNewManagerAppliesDefaults(t *testing.T) {
	manager, err := NewManager(&ManagerOptions{
		Key:                  "12345678901234567890123456789012",
		DisableDefaultDriver: true,
	})
	if err != nil {
		t.Fatalf("NewManager with zero Lifetime/GcInterval failed: %v", err)
	}
	defer func() { _ = manager.Close() }()

	if manager.Lifetime != DefaultLifetime {
		t.Fatalf("Lifetime = %d, want default %d", manager.Lifetime, DefaultLifetime)
	}
	if manager.GcInterval != DefaultGcInterval {
		t.Fatalf("GcInterval = %d, want default %d", manager.GcInterval, DefaultGcInterval)
	}
}

func TestManagerCloseClosesDriversAndIsIdempotent(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	if err := manager.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}

	d.mu.Lock()
	closes := d.closes
	d.mu.Unlock()
	if closes != 1 {
		t.Fatalf("driver Close called %d times, want 1", closes)
	}
}

func TestManagerExtendIsConcurrencySafe(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)
	defer func() { _ = manager.Close() }()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = manager.Extend(fmt.Sprintf("driver-%d", i), newMemoryDriver())
		}(i)
		go func() {
			defer wg.Done()
			s, err := manager.BuildSession(CookieName, "mock")
			if err != nil {
				t.Errorf("BuildSession failed: %v", err)
				return
			}
			manager.ReleaseSession(s)
		}()
	}
	wg.Wait()
}

func TestSessionInvalidateFreshSessionWithFileDriver(t *testing.T) {
	manager, err := NewManager(&ManagerOptions{
		Key:                  "12345678901234567890123456789012",
		Lifetime:             10,
		GcInterval:           10,
		DisableDefaultDriver: true,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() { _ = manager.Close() }()
	// t.TempDir() itself is 0755 on some platforms, which the driver's
	// directory check rejects — let it create its own 0700 subdirectory.
	if err = manager.Extend("file", driver.NewFile(filepath.Join(t.TempDir(), "sessions"), 10)); err != nil {
		t.Fatalf("Extend failed: %v", err)
	}

	s, err := manager.BuildSession(CookieName, "file")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	defer manager.ReleaseSession(s)
	s.Start()

	// A never-persisted session must be invalidatable (session fixation guard
	// on login happens before anything was saved).
	if err = s.Invalidate(); err != nil {
		t.Fatalf("Invalidate on a fresh session failed: %v", err)
	}
}

func TestSessionAllReturnsCopy(t *testing.T) {
	manager := testManagerWithDriver(t, newMemoryDriver())

	s, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	defer manager.ReleaseSession(s)
	s.Start()
	s.Put("key", "value")

	all := s.All()
	all["key"] = "tampered"
	all["extra"] = true

	if got := s.Get("key"); got != "value" {
		t.Fatalf("mutating All() result must not affect the session, got %v", got)
	}
	if s.Exists("extra") {
		t.Fatal("mutating All() result must not add attributes")
	}
}

func TestSessionNonDirtySaveFailsOnStoreOutage(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	// Persist a session with data
	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	s1.Put("key", "value")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	id := s1.GetID()
	manager.ReleaseSession(s1)

	// Re-open it, then degrade the store: Touch and Read fail with
	// non-not-found errors while Write would still succeed.
	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(id)
	s2.Start()
	d.mu.Lock()
	d.failTouch = true
	d.failRead = true
	dataBefore := d.data[id]
	d.mu.Unlock()

	// A read-only save during the outage must fail loudly, not overwrite
	// the stored session with an empty one.
	if err = s2.Save(); err == nil {
		t.Fatal("expected Save to fail during a store outage")
	}
	manager.ReleaseSession(s2)

	d.mu.Lock()
	dataAfter := d.data[id]
	d.mu.Unlock()
	if dataAfter != dataBefore {
		t.Fatal("store outage during a read-only save must not rewrite session data")
	}
}

func TestSessionDirtySaveFailsOnReadOutage(t *testing.T) {
	d := newMemoryDriver()
	manager := testManagerWithDriver(t, d)

	s1, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s1.Start()
	s1.Put("existing", "data")
	if err = s1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	id := s1.GetID()
	manager.ReleaseSession(s1)

	s2, err := manager.BuildSession(CookieName, "mock")
	if err != nil {
		t.Fatalf("BuildSession failed: %v", err)
	}
	s2.SetID(id)
	s2.Start()
	s2.Put("new", "value")

	// Read fails during the merge: Save must not merge against an empty
	// base (which would drop "existing") — it must fail instead.
	d.mu.Lock()
	d.failRead = true
	dataBefore := d.data[id]
	d.mu.Unlock()

	if err = s2.Save(); err == nil {
		t.Fatal("expected dirty Save to fail when the merge base cannot be read")
	}
	manager.ReleaseSession(s2)

	d.mu.Lock()
	dataAfter := d.data[id]
	d.mu.Unlock()
	if dataAfter != dataBefore {
		t.Fatal("failed merge must leave the stored session untouched")
	}
}

func TestSessionSaveRefusesToResurrectDestroyedSession(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		name := "nonDirty"
		if dirty {
			name = "dirty"
		}
		t.Run(name, func(t *testing.T) {
			d := newMemoryDriver()
			manager := testManagerWithDriver(t, d)

			// Establish a session with data
			seed, err := manager.BuildSession(CookieName, "mock")
			if err != nil {
				t.Fatalf("BuildSession failed: %v", err)
			}
			seed.Start()
			seed.Put("user", "before-logout")
			if err = seed.Save(); err != nil {
				t.Fatalf("seed Save failed: %v", err)
			}
			oldID := seed.GetID()
			manager.ReleaseSession(seed)

			// Request A loads the session (slow request, still in flight)
			a, err := manager.BuildSession(CookieName, "mock")
			if err != nil {
				t.Fatalf("BuildSession failed: %v", err)
			}
			a.SetID(oldID)
			a.Start()
			if dirty {
				a.Put("late", "write")
			}

			// Request B logs the user out concurrently: Invalidate destroys
			// the old ID and saves under a fresh one.
			b, err := manager.BuildSession(CookieName, "mock")
			if err != nil {
				t.Fatalf("BuildSession failed: %v", err)
			}
			b.SetID(oldID)
			b.Start()
			if err = b.Invalidate(); err != nil {
				t.Fatalf("Invalidate failed: %v", err)
			}
			if err = b.Save(); err != nil {
				t.Fatalf("B Save failed: %v", err)
			}
			manager.ReleaseSession(b)

			// A finishes last: its save must fail instead of resurrecting
			// the destroyed session ID.
			if err = a.Save(); !errors.Is(err, ErrSessionDestroyed) {
				t.Fatalf("Save = %v, want ErrSessionDestroyed", err)
			}
			manager.ReleaseSession(a)

			d.mu.Lock()
			_, resurrected := d.data[oldID]
			d.mu.Unlock()
			if resurrected {
				t.Fatal("destroyed session ID was written back to the store")
			}
		})
	}
}
