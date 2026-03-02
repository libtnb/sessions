package sessions

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type memoryDriver struct {
	mu        sync.Mutex
	data      map[string]string
	writes    int
	touches   int
	failWrite bool
}

func newMemoryDriver() *memoryDriver {
	return &memoryDriver{
		data: make(map[string]string),
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

func (d *memoryDriver) Touch(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.data[id]; !ok {
		return fmt.Errorf("session [%s] not found", id)
	}
	d.touches++
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
