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
// The session cookie is (re)sent on every response whose session was saved
// successfully, so its expiry slides along with the server-side lifetime.
// For streaming responses the session is saved right before the first byte
// goes out; changes made after that are still persisted when the handler
// returns, but can no longer affect the cookie.
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

			// saveAndSetCookie persists the session and, on success, (re)sends
			// the session cookie so its expiry slides. It runs exactly once:
			// either right before the first flush of a streaming response, or
			// after the handler returns.
			saved := false
			saveAndSetCookie := func() {
				if saved {
					return
				}
				saved = true

				if err := s.Save(); err != nil {
					manager.Logger().Error("session save failed", "error", err)
					return
				}

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
			writer := newResponseWriter(w)
			writer.beforeHeader = saveAndSetCookie
			next.ServeHTTP(writer, r)

			// Save session and set the cookie (no-op if a streaming flush
			// already did it)
			saveAndSetCookie()
			if writer.headerSent && s.IsDirty() {
				// The handler modified the session after the header went out
				// (e.g. during a streaming response): persist the late
				// changes; the cookie for this response is already fixed.
				if err := s.Save(); err != nil {
					manager.Logger().Error("session save failed", "error", err)
				}
			}

			// Flush response and release session
			writer.Flush()
			manager.ReleaseSession(s)
		})
	}
}
