package sessions

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/libtnb/securecookie"

	"github.com/libtnb/sessions/driver"
)

var (
	CtxKey     = "session" // session default context key
	CookieName = "session" // session default cookie name
)

// Sentinel errors returned by the Manager; test with errors.Is.
var (
	ErrSessionNotFound    = errors.New("session not found")
	ErrDriverNotSet       = errors.New("driver is not set")
	ErrDriverNotSupported = errors.New("driver not supported")
	ErrDriverExists       = errors.New("driver already exists")
)

const (
	// DefaultLifetime is used when ManagerOptions.Lifetime is not positive.
	DefaultLifetime = 120 // minutes
	// DefaultGcInterval is used when ManagerOptions.GcInterval is not positive.
	DefaultGcInterval = 30 // minutes
)

type ManagerOptions struct {
	// Key is the 32 bytes string used to encrypt session data.
	Key string
	// Lifetime is the session lifetime in minutes. Defaults to DefaultLifetime.
	Lifetime int
	// GcInterval is the session garbage collection interval in minutes.
	// Defaults to DefaultGcInterval.
	GcInterval int
	// DisableDefaultDriver disables the default file driver if set to true.
	DisableDefaultDriver bool
	// Logger receives background errors (garbage collection, middleware
	// saves). Defaults to slog.Default().
	Logger *slog.Logger
}

type Manager struct {
	Codec      securecookie.Codec
	Lifetime   int
	GcInterval int

	logger         *slog.Logger
	driversMu      sync.RWMutex
	drivers        map[string]driver.Driver
	sessionPool    sync.Pool
	sessionLocksMu sync.Mutex
	sessionLocks   map[string]*sessionLock
	gcDone         chan struct{}
	closeOnce      sync.Once
}

type sessionLock struct {
	mu   sync.Mutex
	refs int
}

// NewManager creates a new session manager.
func NewManager(option *ManagerOptions) (*Manager, error) {
	lifetime := option.Lifetime
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}
	gcInterval := option.GcInterval
	if gcInterval <= 0 {
		gcInterval = DefaultGcInterval
	}
	logger := option.Logger
	if logger == nil {
		logger = slog.Default()
	}

	codec, err := securecookie.New([]byte(option.Key), &securecookie.Options{
		MaxAge:     int64(lifetime) * 60,
		Serializer: securecookie.GobEncoder{},
	})
	if err != nil {
		return nil, err
	}
	manager := &Manager{
		Codec:        codec,
		Lifetime:     lifetime,
		GcInterval:   gcInterval,
		logger:       logger,
		drivers:      make(map[string]driver.Driver),
		sessionLocks: make(map[string]*sessionLock),
		gcDone:       make(chan struct{}),
		sessionPool: sync.Pool{New: func() any {
			return &Session{
				attributes: make(map[string]any),
				puts:       make(map[string]any),
				forgets:    make(map[string]bool),
			}
		},
		},
	}

	if !option.DisableDefaultDriver {
		return manager, manager.createDefaultDriver()
	}
	return manager, nil
}

// BuildSession acquires a pooled session bound to the given driver. Release
// it with ReleaseSession when the request is done.
func (m *Manager) BuildSession(name string, driver ...string) (*Session, error) {
	handler, err := m.driver(driver...)
	if err != nil {
		return nil, err
	}

	session := m.AcquireSession()
	session.id = session.generateSessionID()
	session.name = name
	session.codec = m.Codec
	session.driver = handler
	session.manager = m

	return session, nil
}

// GetSession returns the session stored in the request context, keyed by
// CtxKey unless a custom key is given.
func (m *Manager) GetSession(r *http.Request, key ...any) (*Session, error) {
	if len(key) == 0 {
		key = append(key, CtxKey)
	}
	session, ok := r.Context().Value(key[0]).(*Session)
	if !ok {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

// HasSession reports whether the request context carries a session.
func (m *Manager) HasSession(r *http.Request, key ...any) bool {
	if len(key) == 0 {
		key = append(key, CtxKey)
	}
	_, ok := r.Context().Value(key[0]).(*Session)
	return ok
}

// Extend registers a custom driver under the given name and starts its
// garbage collection timer. It is safe for concurrent use.
func (m *Manager) Extend(name string, handler driver.Driver) error {
	m.driversMu.Lock()
	if m.drivers[name] != nil {
		m.driversMu.Unlock()
		return fmt.Errorf("%w: [%s]", ErrDriverExists, name)
	}
	m.drivers[name] = handler
	m.driversMu.Unlock()

	m.startGcTimer(handler)
	return nil
}

// Close stops the garbage collection timers and closes all registered
// drivers. It is idempotent.
func (m *Manager) Close() error {
	var errs []error
	m.closeOnce.Do(func() {
		close(m.gcDone)

		m.driversMu.RLock()
		defer m.driversMu.RUnlock()
		for name, handler := range m.drivers {
			if err := handler.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close driver [%s]: %w", name, err))
			}
		}
	})
	return errors.Join(errs...)
}

// Logger returns the logger the manager was configured with.
func (m *Manager) Logger() *slog.Logger {
	return m.logger
}

// AcquireSession takes a session from the pool.
func (m *Manager) AcquireSession() *Session {
	session := m.sessionPool.Get().(*Session)
	return session
}

// ReleaseSession resets the session and returns it to the pool. The session
// must not be used afterwards.
func (m *Manager) ReleaseSession(session *Session) {
	session.reset()
	m.sessionPool.Put(session)
}

// LockSession locks the given session ID so that concurrent saves of the
// same session are serialized.
func (m *Manager) LockSession(id string) {
	m.sessionLocksMu.Lock()
	lock, ok := m.sessionLocks[id]
	if !ok {
		lock = &sessionLock{}
		m.sessionLocks[id] = lock
	}
	lock.refs++
	m.sessionLocksMu.Unlock()

	lock.mu.Lock()
}

// UnlockSession releases the lock for the given session ID.
func (m *Manager) UnlockSession(id string) {
	m.sessionLocksMu.Lock()
	lock, ok := m.sessionLocks[id]
	if !ok {
		m.sessionLocksMu.Unlock()
		return
	}
	lock.refs--
	shouldDelete := lock.refs == 0
	m.sessionLocksMu.Unlock()

	lock.mu.Unlock()

	if shouldDelete {
		m.sessionLocksMu.Lock()
		if current, ok := m.sessionLocks[id]; ok && current == lock && lock.refs == 0 {
			delete(m.sessionLocks, id)
		}
		m.sessionLocksMu.Unlock()
	}
}

func (m *Manager) driver(name ...string) (driver.Driver, error) {
	driverName := "default"
	if len(name) > 0 {
		driverName = name[0]
	}

	if driverName == "" {
		return nil, ErrDriverNotSet
	}

	m.driversMu.RLock()
	handler := m.drivers[driverName]
	m.driversMu.RUnlock()

	if handler == nil {
		return nil, fmt.Errorf("%w: [%s]", ErrDriverNotSupported, driverName)
	}

	return handler, nil
}

func (m *Manager) startGcTimer(driver driver.Driver) {
	ticker := time.NewTicker(time.Duration(m.GcInterval) * time.Minute)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-m.gcDone:
				return
			case <-ticker.C:
				if err := driver.Gc(m.Lifetime * 60); err != nil {
					m.logger.Error("session gc failed", "error", err)
				}
			}
		}
	}()
}

func (m *Manager) createDefaultDriver() error {
	return m.Extend("default", driver.NewFile("", m.Lifetime))
}
