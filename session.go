package sessions

import (
	stdmaps "maps"
	"slices"

	"github.com/jaevor/go-nanoid"
	"github.com/spf13/cast"

	"github.com/libtnb/securecookie"
	"github.com/libtnb/sessions/driver"
	"github.com/libtnb/utils/maps"
)

type Session struct {
	id         string
	name       string
	attributes map[string]any
	codec      securecookie.Codec
	driver     driver.Driver
	manager    *Manager // 用于 Save 时加锁
	started    bool
	dirty      bool
	flushed    bool            // Flush 或 Regenerate 被调用，Save 时不合并
	puts       map[string]any  // 本次请求中 Put 的键值
	forgets    map[string]bool // 本次请求中 Forget 的键
}

func (s *Session) All() map[string]any {
	return s.attributes
}

func (s *Session) Exists(key string) bool {
	return maps.Exists(s.attributes, key)
}

func (s *Session) Flash(key string, value any) *Session {
	s.Put(key, value)

	old := s.Get("_flash.new", []any{}).([]any)
	s.Put("_flash.new", append(old, key))

	s.removeFromOldFlashData(key)
	return s
}

func (s *Session) Flush() *Session {
	s.attributes = make(map[string]any)
	s.puts = make(map[string]any)
	s.forgets = make(map[string]bool)
	s.flushed = true
	s.dirty = true
	return s
}

func (s *Session) Forget(keys ...string) *Session {
	maps.Forget(s.attributes, keys...)
	for _, key := range keys {
		s.forgets[key] = true
		delete(s.puts, key)
	}
	s.dirty = true
	return s
}

func (s *Session) Get(key string, defaultValue ...any) any {
	return maps.Get(s.attributes, key, defaultValue...)
}

func (s *Session) GetID() string {
	return s.id
}

func (s *Session) GetName() string {
	return s.name
}

func (s *Session) Has(key string) bool {
	val, ok := s.attributes[key]
	if !ok {
		return false
	}

	return val != nil
}

func (s *Session) Invalidate() error {
	s.Flush()
	return s.migrate(true)
}

func (s *Session) IsDirty() bool {
	return s.dirty
}

func (s *Session) Keep(keys ...string) *Session {
	s.mergeNewFlashes(keys...)
	s.removeFromOldFlashData(keys...)
	return s
}

func (s *Session) Missing(key string) bool {
	return !s.Exists(key)
}

func (s *Session) Now(key string, value any) *Session {
	s.Put(key, value)

	old := s.Get("_flash.old", []any{}).([]any)
	s.Put("_flash.old", append(old, key))

	return s
}

func (s *Session) Only(keys []string) map[string]any {
	return maps.Only(s.attributes, keys...)
}

func (s *Session) Pull(key string, def ...any) any {
	s.forgets[key] = true
	delete(s.puts, key)
	s.dirty = true
	return maps.Pull(s.attributes, key, def...)
}

func (s *Session) Put(key string, value any) *Session {
	s.attributes[key] = value
	s.puts[key] = value
	delete(s.forgets, key)
	s.dirty = true
	return s
}

func (s *Session) Reflash() *Session {
	old := cast.ToStringSlice(s.Get("_flash.old", []any{}).([]any))
	s.mergeNewFlashes(old...)
	s.Put("_flash.old", []any{})
	return s
}

func (s *Session) Regenerate(destroy ...bool) error {
	return s.migrate(destroy...)
}

func (s *Session) Remove(key string) any {
	return s.Pull(key)
}

func (s *Session) Save() error {
	s.ageFlashData()

	if !s.dirty {
		return nil
	}

	// 短暂加锁，仅在合并写入期间持有
	if s.manager != nil {
		s.manager.LockSession(s.GetID())
		defer s.manager.UnlockSession(s.GetID())
	}

	var final map[string]any

	if s.flushed {
		// Flush 或 Regenerate 被调用，直接使用当前状态
		final = s.attributes
	} else {
		// 重新读取数据库最新状态，合并本次变更
		latest := s.readFromHandler()
		if latest == nil {
			latest = make(map[string]any)
		}
		for key := range s.forgets {
			delete(latest, key)
		}
		for key, value := range s.puts {
			latest[key] = value
		}
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

func (s *Session) SetID(id string) *Session {
	if s.isValidID(id) {
		s.id = id
	} else {
		s.id = s.generateSessionID()
	}

	return s
}

func (s *Session) SetName(name string) *Session {
	s.name = name
	return s
}

func (s *Session) Start() bool {
	s.loadSession()
	s.started = true
	return s.started
}

func (s *Session) IsStarted() bool {
	return s.started
}

func (s *Session) generateSessionID() string {
	alphabet := `0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz`
	generator := nanoid.MustCustomASCII(alphabet, 32)
	return generator()
}

func (s *Session) isValidID(id string) bool {
	return len(id) == 32
}

func (s *Session) loadSession() {
	data := s.readFromHandler()
	if data != nil {
		stdmaps.Copy(s.attributes, data)
	}
}

func (s *Session) migrate(destroy ...bool) error {
	shouldDestroy := false
	if len(destroy) > 0 {
		shouldDestroy = destroy[0]
	}

	if shouldDestroy {
		err := s.driver.Destroy(s.GetID())
		if err != nil {
			return err
		}
	}

	s.id = s.generateSessionID()
	s.dirty = true
	s.flushed = true // 新 session ID，不需要合并
	return nil
}

func (s *Session) readFromHandler() map[string]any {
	value, err := s.driver.Read(s.GetID())
	if err != nil {
		return nil
	}

	var data map[string]any
	if _, err = s.codec.Decode(s.GetName(), value, &data); err != nil {
		return nil
	}
	return data
}

func (s *Session) ageFlashData() {
	old := cast.ToStringSlice(s.Get("_flash.old", []any{}).([]any))
	newFlash := s.Get("_flash.new", []any{}).([]any)

	if len(old) == 0 && len(newFlash) == 0 {
		return
	}

	if len(old) > 0 {
		s.Forget(old...)
	}

	s.Put("_flash.old", newFlash)
	s.Put("_flash.new", []any{})
}

func (s *Session) mergeNewFlashes(keys ...string) {
	values := s.Get("_flash.new", []any{}).([]any)
	for _, key := range keys {
		if !slices.Contains(values, any(key)) {
			values = append(values, key)
		}
	}

	s.Put("_flash.new", values)
}

func (s *Session) removeFromOldFlashData(keys ...string) {
	old := s.Get("_flash.old", []any{}).([]any)
	for _, key := range keys {
		old = slices.DeleteFunc(old, func(i any) bool {
			return cast.ToString(i) == key
		})
	}
	s.Put("_flash.old", old)
}

func (s *Session) reset() {
	s.id = ""
	s.name = ""
	s.attributes = make(map[string]any)
	s.puts = make(map[string]any)
	s.forgets = make(map[string]bool)
	s.codec = nil
	s.driver = nil
	s.manager = nil
	s.started = false
	s.dirty = false
	s.flushed = false
}
