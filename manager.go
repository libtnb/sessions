package sessions

import (
	"fmt"
	"log"
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

type ManagerOptions struct {
	// 32 bytes string to encrypt session data
	Key string
	// session lifetime in minutes
	Lifetime int
	// session garbage collection interval in minutes
	GcInterval int
	// Disable default file driver if set to true
	DisableDefaultDriver bool
}

type Manager struct {
	Codec        securecookie.Codec
	Lifetime     int
	GcInterval   int
	drivers      map[string]driver.Driver
	sessionPool  sync.Pool
	sessionLocks sync.Map // sessionID → *sync.Mutex
}

// NewManager creates a new session manager.
func NewManager(option *ManagerOptions) (*Manager, error) {
	codec, err := securecookie.New([]byte(option.Key), &securecookie.Options{
		MaxAge:     int64(option.Lifetime) * 60,
		Serializer: securecookie.GobEncoder{},
	})
	if err != nil {
		return nil, err
	}
	manager := &Manager{
		Codec:      codec,
		Lifetime:   option.Lifetime,
		GcInterval: option.GcInterval,
		drivers:    make(map[string]driver.Driver),
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

func (m *Manager) GetSession(r *http.Request, key ...any) (*Session, error) {
	if len(key) == 0 {
		key = append(key, CtxKey)
	}
	session, ok := r.Context().Value(key[0]).(*Session)
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	return session, nil
}

func (m *Manager) HasSession(r *http.Request, key ...any) bool {
	if len(key) == 0 {
		key = append(key, CtxKey)
	}
	_, ok := r.Context().Value(key[0]).(*Session)
	return ok
}

func (m *Manager) Extend(driver string, handler driver.Driver) error {
	if m.drivers[driver] != nil {
		return fmt.Errorf("driver [%s] already exists", driver)
	}
	m.drivers[driver] = handler
	m.startGcTimer(m.drivers[driver])
	return nil
}

func (m *Manager) AcquireSession() *Session {
	session := m.sessionPool.Get().(*Session)
	return session
}

func (m *Manager) ReleaseSession(session *Session) {
	session.reset()
	m.sessionPool.Put(session)
}

// LockSession 对指定 session ID 加锁
func (m *Manager) LockSession(id string) {
	mu, _ := m.sessionLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
}

// UnlockSession 释放指定 session ID 的锁
func (m *Manager) UnlockSession(id string) {
	if mu, ok := m.sessionLocks.Load(id); ok {
		mu.(*sync.Mutex).Unlock()
	}
}

func (m *Manager) driver(name ...string) (driver.Driver, error) {
	var driverName string
	if len(name) > 0 {
		driverName = name[0]
	} else {
		driverName = "default"
	}

	if driverName == "" {
		return nil, fmt.Errorf("driver is not set")
	}

	if m.drivers[driverName] == nil {
		return nil, fmt.Errorf("driver [%s] not supported", driverName)
	}

	return m.drivers[driverName], nil
}

func (m *Manager) startGcTimer(driver driver.Driver) {
	ticker := time.NewTicker(time.Duration(m.GcInterval) * time.Minute)

	go func() {
		for range ticker.C {
			if err := driver.Gc(m.Lifetime * 60); err != nil {
				log.Printf("session gc error: %v\n", err)
			}
		}
	}()
}

func (m *Manager) createDefaultDriver() error {
	return m.Extend("default", driver.NewFile("", 120))
}
