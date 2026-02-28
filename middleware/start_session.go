package middleware

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/libtnb/sessions"
)

// StartSession is an example middleware that starts a session for each request.
// If this middleware not suitable for your application, you can create your own.
func StartSession(manager *sessions.Manager, driver ...string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check if session exists
			_, ok := r.Context().Value(sessions.CtxKey).(*sessions.Session)
			if ok {
				next.ServeHTTP(w, r)
				return
			}

			// Build session
			s, err := manager.BuildSession(sessions.CookieName, driver...)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Try to get and decode session ID from cookie
			var sessionID string
			if cookie, err := r.Cookie(s.GetName()); err == nil {
				sessionID = cookie.Value
				s.SetID(cookie.Value)
			}

			// Start session
			s.Start()
			r = r.WithContext(context.WithValue(r.Context(), sessions.CtxKey, s)) //nolint:staticcheck

			// Continue processing request
			writer := newResponseWriter(w)
			next.ServeHTTP(writer, r)

			// Check whether we need to reset session Cookie if session ID has changed
			if s.GetID() != sessionID {
				// Set session cookie in response
				http.SetCookie(w, &http.Cookie{
					Name:     s.GetName(),
					Value:    s.GetID(),
					Expires:  time.Now().Add(time.Duration(manager.Lifetime) * time.Minute),
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
				})
			}

			// Save session (skipped internally if not dirty)
			if err = s.Save(); err != nil {
				log.Printf("session save error: %v", err)
			}

			// Flush response and release session
			writer.Flush()
			manager.ReleaseSession(s)
		})
	}
}
