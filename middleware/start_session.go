package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/libtnb/sessions"
)

// Config customizes the StartSession middleware.
type Config struct {
	// Driver is the session driver name; empty selects the default driver.
	Driver string
	// Cookie, when set, is called with the prepared session cookie before it
	// is written, allowing customization of Path, Domain, Secure, SameSite
	// and the other attributes.
	Cookie func(*http.Cookie)
}

// StartSession is an example middleware that starts a session for each request.
// If this middleware not suitable for your application, you can create your own.
func StartSession(manager *sessions.Manager, driver ...string) func(next http.Handler) http.Handler {
	cfg := Config{}
	if len(driver) > 0 {
		cfg.Driver = driver[0]
	}
	return StartSessionWithConfig(manager, cfg)
}

// StartSessionWithConfig is StartSession with explicit configuration.
//
// The response is not buffered. The session is saved and its cookie (re)sent
// right before the response header goes out: on the handler's first
// WriteHeader, Write or Flush, or when it returns without having written
// anything. The cookie is only sent when that save succeeded, so its expiry
// slides along with the server-side lifetime.
//
// Changes made after the header went out are still persisted when the
// handler returns, but can no longer affect the cookie. A session ID that is
// regenerated that late cannot reach the client and is not stored. When the
// save before the header fails, the session is not saved again.
//
// The first write reads the session, so it must not run concurrently with
// changes to it.
func StartSessionWithConfig(manager *sessions.Manager, cfg Config) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if session exists
			if _, ok := r.Context().Value(sessions.CtxKey).(*sessions.Session); ok {
				next.ServeHTTP(w, r)
				return
			}

			// Build session
			var driverNames []string
			if cfg.Driver != "" {
				driverNames = append(driverNames, cfg.Driver)
			}
			s, err := manager.BuildSession(sessions.CookieName, driverNames...)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Adopt the session ID from the cookie when present and valid
			if cookie, err := r.Cookie(s.GetName()); err == nil {
				s.SetID(cookie.Value)
			}

			// Start session
			s.Start()
			r = r.WithContext(context.WithValue(r.Context(), sessions.CtxKey, s)) //nolint:staticcheck

			// sentID is the session ID the cookie went out with; it stays
			// empty when the session could not be saved.
			var sentID string
			saveAndSetCookie := func() {
				if err := s.Save(); err != nil {
					manager.Logger().Error("session save failed", "error", err)
					return
				}
				sentID = s.GetID()

				cookie := &http.Cookie{
					Name:     s.GetName(),
					Value:    s.GetID(),
					MaxAge:   manager.Lifetime * 60,
					Expires:  time.Now().Add(time.Duration(manager.Lifetime) * time.Minute),
					Path:     "/",
					HttpOnly: true,
					Secure:   r.TLS != nil,
					SameSite: http.SameSiteLaxMode,
				}
				if cfg.Cookie != nil {
					cfg.Cookie(cookie)
				}
				http.SetCookie(w, cookie)
			}

			// Continue processing request
			writer := &responseWriter{ResponseWriter: w, beforeHeader: saveAndSetCookie}
			next.ServeHTTP(writer, r)

			if !writer.commit() && sentID != "" {
				// The header went out while the handler was still running:
				// the cookie is fixed, only data can still be stored.
				switch {
				case s.GetID() != sentID:
					manager.Logger().Error("session ID changed after the response header was sent, the client keeps the previous one")
				case s.IsDirty():
					if err := s.Save(); err != nil {
						manager.Logger().Error("session save failed", "error", err)
					}
				}
			}

			manager.ReleaseSession(s)
		})
	}
}
