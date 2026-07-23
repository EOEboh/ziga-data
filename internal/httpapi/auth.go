package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	mailer "github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/store"
)

const (
	verifyTokenTTL = 24 * time.Hour
	resetTokenTTL  = 1 * time.Hour
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt only considers the first 72 bytes
)

// userJSON is the public shape of an account (never includes the password
// hash).
type userJSON struct {
	ID            int64  `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

func toUserJSON(u *store.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, EmailVerified: u.Verified()}
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validEmail(s string) bool {
	if len(s) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s
}

// handleSignup creates an unverified email+password account and emails a
// verification link. It does not start a session — the account is inactive
// until verified.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := normalizeEmail(req.Email)
	if !validEmail(email) {
		httpError(w, http.StatusBadRequest, "enter a valid email address")
		return
	}
	if len(req.Password) < minPasswordLen || len(req.Password) > maxPasswordLen {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("password must be %d–%d characters", minPasswordLen, maxPasswordLen))
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	u, err := s.store.CreateUser(r.Context(), email, hash)
	if err != nil {
		// Most likely the UNIQUE email constraint.
		if existing, gerr := s.store.GetUserByEmail(r.Context(), email); gerr == nil && existing != nil {
			httpError(w, http.StatusConflict, "an account with this email already exists")
			return
		}
		s.log.Error("create user", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.sendVerification(r.Context(), u); err != nil {
		s.log.Error("send verification", "err", err)
		// The account exists; surface a soft error so the user can request a resend.
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "verification_sent",
		"user":   toUserJSON(u),
	})
}

// sendVerification mints a single-use token, stores its hash, and emails the
// verification link.
func (s *Server) sendVerification(ctx context.Context, u *store.User) error {
	token, err := auth.RandomToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateAuthToken(ctx, u.ID, store.TokenVerifyEmail, auth.HashToken(token), time.Now().Add(verifyTokenTTL)); err != nil {
		return err
	}
	link := s.baseURL + "/api/auth/verify?token=" + token
	return s.mailer.Send(ctx, mailer.Message{
		To:      u.Email,
		Subject: "Verify your Ziga account",
		Text:    "Welcome to Ziga. Confirm your email to activate your account:\n\n" + link + "\n\nThis link expires in 24 hours.",
		HTML:    fmt.Sprintf(`<p>Welcome to Ziga. Confirm your email to activate your account:</p><p><a href="%s">Verify my email</a></p><p>This link expires in 24 hours.</p>`, link),
	})
}

// handleVerifyEmail consumes a verification token (from the emailed link) and
// marks the account verified, then redirects into the app. This is a GET on a
// capability token, so it carries no CSRF requirement.
func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, s.baseURL+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	uid, err := s.store.ConsumeAuthToken(r.Context(), store.TokenVerifyEmail, auth.HashToken(token))
	if err != nil {
		http.Redirect(w, r, s.baseURL+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	if err := s.store.MarkEmailVerified(r.Context(), uid); err != nil {
		s.log.Error("mark verified", "err", err)
		http.Redirect(w, r, s.baseURL+"/login?verify_error=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.baseURL+"/login?verified=1", http.StatusSeeOther)
}

// handleLogin authenticates an email+password user and starts a session.
// Unverified accounts are refused. Errors are deliberately generic to avoid
// revealing which emails are registered.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := normalizeEmail(req.Email)
	u, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil || !auth.CheckPassword(u.PasswordHash, req.Password) {
		httpError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if !u.Verified() {
		httpError(w, http.StatusForbidden, "please verify your email before signing in")
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		s.log.Error("start session", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserJSON(u)})
}

// handleLogout destroys the current session and clears the cookie. Safe to
// call when already logged out.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if derr := s.store.DeleteSession(r.Context(), auth.HashToken(c.Value)); derr != nil {
			s.log.Error("delete session", "err", derr)
		}
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// handleForgotPassword emails a reset link when the address has an account. It
// always returns 200 so the endpoint can't be used to enumerate accounts.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := normalizeEmail(req.Email)
	if u, err := s.store.GetUserByEmail(r.Context(), email); err == nil && u != nil {
		if serr := s.sendPasswordReset(r.Context(), u); serr != nil {
			s.log.Error("send reset", "err", serr)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) sendPasswordReset(ctx context.Context, u *store.User) error {
	token, err := auth.RandomToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateAuthToken(ctx, u.ID, store.TokenPasswordReset, auth.HashToken(token), time.Now().Add(resetTokenTTL)); err != nil {
		return err
	}
	link := s.baseURL + "/reset?token=" + token
	return s.mailer.Send(ctx, mailer.Message{
		To:      u.Email,
		Subject: "Reset your Ziga password",
		Text:    "Reset your Ziga password:\n\n" + link + "\n\nThis link expires in 1 hour. If you didn't request this, ignore this email.",
		HTML:    fmt.Sprintf(`<p>Reset your Ziga password:</p><p><a href="%s">Choose a new password</a></p><p>This link expires in 1 hour. If you didn't request this, ignore this email.</p>`, link),
	})
}

// handleResetPassword consumes a reset token and sets a new password. Since the
// token proves control of the email, the account is also marked verified.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ Token, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Password) < minPasswordLen || len(req.Password) > maxPasswordLen {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("password must be %d–%d characters", minPasswordLen, maxPasswordLen))
		return
	}
	uid, err := s.store.ConsumeAuthToken(r.Context(), store.TokenPasswordReset, auth.HashToken(req.Token))
	if err != nil {
		httpError(w, http.StatusBadRequest, "this reset link is invalid or has expired")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.SetPasswordHash(r.Context(), uid, hash); err != nil {
		s.log.Error("set password", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Recovering via an emailed link proves ownership, so verify too.
	if err := s.store.MarkEmailVerified(r.Context(), uid); err != nil {
		s.log.Error("mark verified on reset", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "password_reset"})
}

// handleMe returns the current session's user (or authenticated:false) plus the
// public config the frontend needs (Google client id / Picker key) and the
// user's connection status. Public route: never 401, so the SPA can bootstrap.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"config": map[string]any{
			"google_oauth":          s.oauth != nil && s.oauth.Configured(),
			"google_client_id":      s.googleClientID(),
			"google_picker_api_key": s.cfg.GooglePickerAPIKey,
			"google_project_number": s.cfg.GoogleProjectNumber,
		},
	}
	uid, ok := s.sessionUser(r)
	if !ok {
		resp["authenticated"] = false
		resp["user"] = nil
		writeJSON(w, http.StatusOK, resp)
		return
	}
	u, err := s.store.GetUser(r.Context(), uid)
	if err != nil {
		resp["authenticated"] = false
		resp["user"] = nil
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["authenticated"] = true
	resp["user"] = toUserJSON(u)
	resp["google_connected"] = s.googleConnected(r, uid)
	resp["sheet_connected"] = s.sheetConnected(r.Context(), uid)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) googleClientID() string {
	if s.oauth == nil {
		return ""
	}
	return s.oauth.ClientID()
}

// googleConnected reports whether the user has a healthy (non-broken) Google
// link.
func (s *Server) googleConnected(r *http.Request, uid int64) bool {
	acct, err := s.store.GetOAuthAccount(r.Context(), uid, googleProvider)
	return err == nil && !acct.Broken()
}
