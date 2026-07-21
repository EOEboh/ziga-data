package httpapi

import (
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
)

const (
	sessionCookie = "ziga_session"
	csrfCookie    = "ziga_csrf"
	sessionTTL    = 30 * 24 * time.Hour
)

// setSessionCookie writes the session token cookie. HttpOnly (JS can't read
// it), SameSite=Lax (sent on top-level navigations, blocks cross-site POSTs),
// Secure when the app is served over https.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// clearSessionCookie expires the session cookie (logout).
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(0, 0), MaxAge: -1,
	})
}

// startSession creates a server-side session for the user and sets the cookie.
// The cookie holds the opaque token; the store keys the session by its hash.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.RandomToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateSession(r.Context(), auth.HashToken(token), userID, time.Now().Add(sessionTTL)); err != nil {
		return err
	}
	s.setSessionCookie(w, token)
	return nil
}

// requireAuth gates a handler on a valid session, injecting the user id into
// the request context. Missing/expired sessions get 401.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := s.sessionUser(r)
		if !ok {
			httpError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), uid)))
	})
}

// sessionUser resolves the request's session cookie to a user id, or false.
func (s *Server) sessionUser(r *http.Request) (int64, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return 0, false
	}
	sess, err := s.store.GetSession(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return 0, false
	}
	return sess.UserID, true
}

// csrf enforces a signed double-submit token on unsafe methods and makes sure
// a fresh CSRF cookie is present for the client to echo. Applied to every /api
// route (public and protected).
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(csrfCookie)
		cookieVal := ""
		if err == nil {
			cookieVal = c.Value
		}
		if cookieVal == "" || !auth.ValidCSRFToken(s.sessionSecret, cookieVal) {
			// Issue (or reissue) a token; the client reads it from the cookie
			// and sends it back on the next unsafe request.
			tok, terr := auth.NewCSRFToken(s.sessionSecret)
			if terr != nil {
				httpError(w, http.StatusInternalServerError, "internal error")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: csrfCookie, Value: tok, Path: "/",
				HttpOnly: false, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
			})
			cookieVal = tok
		}
		if unsafeMethod(r.Method) {
			header := r.Header.Get("X-CSRF-Token")
			if !auth.ValidCSRFToken(s.sessionSecret, cookieVal) || !auth.EqualToken(header, cookieVal) {
				httpError(w, http.StatusForbidden, "invalid or missing CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func unsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// rateLimitFor wraps a handler with a specific per-IP limiter (login and
// password-reset get their own stricter budgets, separate from the API one).
func (s *Server) rateLimitFor(l *ipLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.get(clientIP(r)).Allow() {
			httpError(w, http.StatusTooManyRequests, "too many attempts — slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}
