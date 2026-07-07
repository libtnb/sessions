package sessions

import (
	"errors"
	stdmaps "maps"
	"slices"

	"github.com/jaevor/go-nanoid"

	"github.com/libtnb/securecookie"
	"github.com/libtnb/sessions/driver"
)

const (
	sessionIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	sessionIDLength   = 32

	flashNewKey = "_flash.new"
	flashOldKey = "_flash.old"
)

// ErrSessionDestroyed reports that a session which existed when the request
// started was destroyed concurrently (logout, Regenerate(true) or garbage
// collection in another request) before Save could persist it. The save is
// refused: recreating the session would resurrect an invalidated ID.
var ErrSessionDestroyed = errors.New("session destroyed concurrently")

// newSessionID is shared by all sessions; the generator is mutex-guarded
// internally, so creating it once avoids per-request initialization cost.
var newSessionID = nanoid.MustCustomASCII(sessionIDAlphabet, sessionIDLength)

// Session holds the data of a single session for the duration of a request.
// It is not safe for concurrent use by multiple goroutines.
type Session struct {
	id         string
	name       string
	attributes map[string]any
	codec      securecookie.Codec
	driver     driver.Driver
	manager    *Manager // used to serialize Save calls per session ID
	started    bool
	dirty      bool
	loaded     bool            // session data was loaded from the store at Start
	flushed    bool            // Flush or Regenerate was called; Save skips merging
	puts       map[string]any  // keys put during this request
	forgets    map[string]bool // keys forgotten during this request
}

// All returns a copy of the session attributes. Mutating the returned map
// does not affect the session; use Put and Forget to modify it.
func (s *Session) All() map[string]any {
	return stdmaps.Clone(s.attributes)
}

// Exists reports whether the key is present, even if its value is nil.
func (s *Session) Exists(key string) bool {
	_, ok := s.attributes[key]
	return ok
}

// Flash stores a key/value pair that survives until the end of the next
// request, then is removed automatically.
func (s *Session) Flash(key string, value any) *Session {
	s.Put(key, value)
	s.mergeNewFlashes(key)
	s.removeFromOldFlashData(key)
	return s
}

// Flush removes all attributes from the session.
func (s *Session) Flush() *Session {
	s.attributes = make(map[string]any)
	s.puts = make(map[string]any)
	s.forgets = make(map[string]bool)
	s.flushed = true
	s.dirty = true
	return s
}

// Forget removes the given keys from the session.
func (s *Session) Forget(keys ...string) *Session {
	for _, key := range keys {
		delete(s.attributes, key)
		s.forgets[key] = true
		delete(s.puts, key)
	}
	s.dirty = true
	return s
}

// Get returns the value for key, or defaultValue (or nil) when the key is
// missing.
func (s *Session) Get(key string, defaultValue ...any) any {
	if value, ok := s.attributes[key]; ok {
		return value
	}
	if len(defaultValue) > 0 {
		return defaultValue[0]
	}
	return nil
}

// GetID returns the session ID.
func (s *Session) GetID() string {
	return s.id
}

// GetName returns the session name.
func (s *Session) GetName() string {
	return s.name
}

// Has reports whether the key is present with a non-nil value.
func (s *Session) Has(key string) bool {
	val, ok := s.attributes[key]
	if !ok {
		return false
	}

	return val != nil
}

// Invalidate flushes all attributes and regenerates the session ID,
// destroying the previously stored session.
func (s *Session) Invalidate() error {
	s.Flush()
	return s.migrate(true)
}

// IsDirty reports whether the session has unsaved changes.
func (s *Session) IsDirty() bool {
	return s.dirty
}

// Keep extends the given flash keys for one more request.
func (s *Session) Keep(keys ...string) *Session {
	s.mergeNewFlashes(keys...)
	s.removeFromOldFlashData(keys...)
	return s
}

// Missing reports whether the key is absent or nil.
func (s *Session) Missing(key string) bool {
	return !s.Has(key)
}

// Now stores a key/value pair that is removed at the end of the current
// request.
func (s *Session) Now(key string, value any) *Session {
	s.Put(key, value)

	old := s.flashKeys(flashOldKey)
	if !slices.Contains(old, key) {
		old = append(old, key)
	}
	s.putFlashKeys(flashOldKey, old)

	return s
}

// Only returns the subset of attributes with the given keys.
func (s *Session) Only(keys []string) map[string]any {
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := s.attributes[key]; ok {
			result[key] = value
		}
	}
	return result
}

// Pull returns the value for key and removes it from the session.
func (s *Session) Pull(key string, def ...any) any {
	value := s.Get(key, def...)
	s.Forget(key)
	return value
}

// Put stores a key/value pair in the session.
func (s *Session) Put(key string, value any) *Session {
	s.attributes[key] = value
	s.puts[key] = value
	delete(s.forgets, key)
	s.dirty = true
	return s
}

// Reflash extends all current flash data for one more request.
func (s *Session) Reflash() *Session {
	s.mergeNewFlashes(s.flashKeys(flashOldKey)...)
	s.putFlashKeys(flashOldKey, nil)
	return s
}

// Regenerate gives the session a new ID. Pass true to also destroy the
// previously stored session data.
func (s *Session) Regenerate(destroy ...bool) error {
	return s.migrate(destroy...)
}

// Remove removes the key from the session and returns its previous value.
func (s *Session) Remove(key string) any {
	return s.Pull(key)
}

// Save persists the session to the driver and marks it as no longer started.
// Concurrent saves of the same session ID are merged key by key: only the
// keys written or forgotten during this request are applied on top of the
// latest stored state.
func (s *Session) Save() error {
	s.ageFlashData()

	// Hold the per-session lock only while reading and writing the store.
	if s.manager != nil {
		s.manager.LockSession(s.GetID())
		defer s.manager.UnlockSession(s.GetID())
	}

	if !s.dirty {
		// No changes: refresh the store timestamp so GC keeps the active
		// session alive.
		found, err := s.driver.Touch(s.GetID())
		if err != nil {
			// The store failed; writing now could overwrite good data with
			// an empty session, so surface the error instead.
			return err
		}
		if found {
			s.started = false
			return nil
		}
		if s.loaded {
			// The session existed at Start but is gone now: it was destroyed
			// concurrently. Persisting would resurrect an invalidated ID.
			return ErrSessionDestroyed
		}
		// Never persisted — fall through and persist the current state so
		// the session ID stays stable across requests.
	}

	var final map[string]any

	if s.flushed {
		// Flush or Regenerate was called; use the current state as-is.
		final = s.attributes
	} else {
		// Merge this request's changes on top of the latest stored state.
		latest, err := s.readFromHandler()
		if err != nil {
			// Store failure (not a missing session): abort rather than merge
			// against an empty base, which would drop concurrent writes.
			return err
		}
		if latest == nil {
			if s.loaded {
				// Destroyed concurrently since Start; refuse to resurrect it.
				return ErrSessionDestroyed
			}
			latest = make(map[string]any)
		}
		for key := range s.forgets {
			delete(latest, key)
		}
		stdmaps.Copy(latest, s.puts)
		final = latest
	}

	data, err := s.codec.Encode(s.GetName(), final)
	if err != nil {
		return err
	}

	if err = s.driver.Write(s.GetID(), data); err != nil {
		return err
	}

	s.dirty = false
	s.started = false
	return nil
}

// SetID sets the session ID. Invalid IDs (wrong length or characters outside
// [0-9A-Za-z]) are replaced with a newly generated one.
func (s *Session) SetID(id string) *Session {
	if s.isValidID(id) {
		s.id = id
	} else {
		s.id = s.generateSessionID()
	}
	s.loaded = false

	return s
}

// SetName sets the session name.
func (s *Session) SetName(name string) *Session {
	s.name = name
	return s
}

// Start loads the session data from the driver. When no stored session is
// found, a fresh session ID is generated.
func (s *Session) Start() bool {
	if !s.loadSession() {
		s.id = s.generateSessionID()
	}
	s.started = true
	return s.started
}

// IsStarted reports whether the session has been started.
func (s *Session) IsStarted() bool {
	return s.started
}

func (s *Session) generateSessionID() string {
	return newSessionID()
}

func (s *Session) isValidID(id string) bool {
	if len(id) != sessionIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') &&
			(c < 'A' || c > 'Z') &&
			(c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func (s *Session) loadSession() bool {
	// A store failure degrades to a fresh session here; Save's merge path
	// re-checks the store and refuses to overwrite data it cannot read.
	data, _ := s.readFromHandler()
	if data == nil {
		return false
	}
	stdmaps.Copy(s.attributes, data)
	s.loaded = true
	return true
}

func (s *Session) migrate(destroy ...bool) error {
	shouldDestroy := false
	if len(destroy) > 0 {
		shouldDestroy = destroy[0]
	}

	if shouldDestroy {
		if err := s.driver.Destroy(s.GetID()); err != nil {
			return err
		}
	}

	s.id = s.generateSessionID()
	s.dirty = true
	s.loaded = false // the new ID has never been persisted
	s.flushed = true // new session ID, nothing to merge with
	return nil
}

// readFromHandler returns the stored session data. A missing session or
// undecodable payload (corrupt data, rotated key) yields (nil, nil) — both
// mean "start fresh". A store failure is returned as an error so callers
// never mistake an outage for an empty session.
func (s *Session) readFromHandler() (map[string]any, error) {
	value, found, err := s.driver.Read(s.GetID())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	var data map[string]any
	if _, err = s.codec.Decode(s.GetName(), value, &data); err != nil {
		return nil, nil
	}
	return data, nil
}

func (s *Session) ageFlashData() {
	old := s.flashKeys(flashOldKey)
	newFlash := s.flashKeys(flashNewKey)

	if len(old) == 0 && len(newFlash) == 0 {
		return
	}

	if len(old) > 0 {
		s.Forget(old...)
	}

	s.putFlashKeys(flashOldKey, newFlash)
	s.putFlashKeys(flashNewKey, nil)
}

func (s *Session) mergeNewFlashes(keys ...string) {
	values := s.flashKeys(flashNewKey)
	for _, key := range keys {
		if !slices.Contains(values, key) {
			values = append(values, key)
		}
	}

	s.putFlashKeys(flashNewKey, values)
}

func (s *Session) removeFromOldFlashData(keys ...string) {
	old := s.flashKeys(flashOldKey)
	for _, key := range keys {
		old = slices.DeleteFunc(old, func(k string) bool {
			return k == key
		})
	}
	s.putFlashKeys(flashOldKey, old)
}

// flashKeys reads a flash key list, tolerating missing or foreign values so
// a polluted attribute can never panic the session.
func (s *Session) flashKeys(key string) []string {
	switch value := s.attributes[key].(type) {
	case []any:
		keys := make([]string, 0, len(value))
		for _, item := range value {
			if k, ok := item.(string); ok {
				keys = append(keys, k)
			}
		}
		return keys
	case []string:
		return slices.Clone(value)
	default:
		return nil
	}
}

// putFlashKeys stores a flash key list as []any, the canonical on-disk form.
func (s *Session) putFlashKeys(key string, keys []string) {
	values := make([]any, len(keys))
	for i, k := range keys {
		values[i] = k
	}
	s.Put(key, values)
}

// maxRetainedEntries caps the map capacity kept by pooled sessions; larger
// maps are dropped so one oversized session does not pin memory forever.
const maxRetainedEntries = 128

func (s *Session) reset() {
	s.id = ""
	s.name = ""
	s.attributes = resetMap(s.attributes)
	s.puts = resetMap(s.puts)
	s.forgets = resetMap(s.forgets)
	s.codec = nil
	s.driver = nil
	s.manager = nil
	s.started = false
	s.dirty = false
	s.loaded = false
	s.flushed = false
}

// resetMap clears m in place to keep its capacity for reuse, allocating a
// fresh map only when m grew unusually large.
func resetMap[V any](m map[string]V) map[string]V {
	if m == nil || len(m) > maxRetainedEntries {
		return make(map[string]V)
	}
	clear(m)
	return m
}
