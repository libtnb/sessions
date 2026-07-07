# Sessions

Sessions package for Go, use stdlib and secure cookie.

Since the experience for `gorilla/sessions` was s*it, we decided to write our own.

This package refers to the session module in [goravel/framework](https://github.com/goravel/framework).

## Features

- Encrypted session data via [libtnb/securecookie](https://github.com/libtnb/securecookie) (only the session ID is stored in the cookie)
- File driver included; any backend can be plugged in through the `driver.Driver` interface
- Concurrent requests to the same session merge key by key instead of overwriting each other
- Flash data (`Flash`, `Now`, `Keep`, `Reflash`)
- Sliding expiration: both the store timestamp and the cookie are refreshed on every request
- Streaming-friendly middleware (SSE, chunked responses) with a buffered response writer
- Session pooling to keep allocations low

## Installation

```bash
go get github.com/libtnb/sessions
```

## Quick start

```go
package main

import (
	"net/http"

	"github.com/libtnb/sessions"
	"github.com/libtnb/sessions/middleware"
)

func main() {
	manager, err := sessions.NewManager(&sessions.ManagerOptions{
		Key:        "32-bytes-long-secret-key-1234567", // 32 bytes, keep it secret
		Lifetime:   120,                                // minutes, default 120
		GcInterval: 30,                                 // minutes, default 30
	})
	if err != nil {
		panic(err)
	}
	defer manager.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s, err := manager.GetSession(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		views, _ := s.Get("views", 0).(int)
		s.Put("views", views+1)
	})

	http.ListenAndServe(":8080", middleware.StartSession(manager)(mux))
}
```

The middleware starts the session, saves it after the handler returns (merging
concurrent writes), and re-sends the cookie on every successful save so its
expiry slides along with the server-side lifetime.

### Customizing the cookie

```go
handler := middleware.StartSessionWithConfig(manager, middleware.Config{
	Driver: "default",
	Cookie: func(c *http.Cookie) {
		c.Domain = "example.com"
		c.SameSite = http.SameSiteStrictMode
		c.Secure = true // defaults to true automatically on TLS requests
	},
})(mux)
```

## Session API

```go
s.Put("key", "value")          // set a value
s.Get("key", "default")        // get with optional default
s.Has("key")                   // present and non-nil
s.Exists("key")                // present, even if nil
s.Pull("key")                  // get and remove
s.Forget("key1", "key2")       // remove keys
s.Flush()                      // remove everything
s.All()                        // copy of all attributes
s.Only([]string{"k1", "k2"})   // subset of attributes

s.Flash("status", "saved!")    // visible until the end of the next request
s.Now("status", "one-shot")    // visible during the current request only
s.Keep("status")               // extend flash data one more request
s.Reflash()                    // extend all flash data one more request

s.Regenerate()                 // new session ID, keep data
s.Regenerate(true)             // new session ID, destroy old stored data
s.Invalidate()                 // flush data + new ID (use on login/logout)
```

Values are encoded with `encoding/gob`. Custom struct types stored in the
session must be registered once with `gob.Register`.

A `*Session` is bound to a single request and is not safe for concurrent use
by multiple goroutines; cross-request merging is handled by the manager.

## Custom drivers

Implement `driver.Driver` and register it:

```go
type Driver interface {
	Close() error
	Destroy(id string) error
	Gc(maxLifetime int) error
	Read(id string) (data string, found bool, err error)
	Touch(id string) (found bool, err error)
	Write(id string, data string) error
}
```

`Read` and `Touch` report a missing or expired session via `found = false`
with a nil error; a non-nil error means the store itself failed. The session
uses this distinction to stay safe: a missing session may be started fresh,
while on a store failure the save fails loudly instead of overwriting stored
data with an empty session. A session that existed at the start of the
request but disappears before `Save` (a concurrent logout or regeneration)
is never written back — `Save` returns `sessions.ErrSessionDestroyed`.

```go
manager, _ := sessions.NewManager(&sessions.ManagerOptions{
	Key:                  "32-bytes-long-secret-key-1234567",
	DisableDefaultDriver: true, // skip the default file driver
})
_ = manager.Extend("redis", myRedisDriver)

handler := middleware.StartSession(manager, "redis")(mux)
```

The default file driver stores each session as a `0600` file in a dedicated
per-user directory inside `os.TempDir()` (`sessions-<uid>` on Unix), writes
atomically (temp file + rename), and its garbage collector only ever removes
files that look like session data. Before writing it verifies the directory
is a real directory owned by the current user with mode `0700` — a
pre-created symlink or permissive directory in the shared temp dir is
rejected. Pass a custom path via `driver.NewFile(path, minutes)` to store
sessions elsewhere; custom paths are held to the same check.
