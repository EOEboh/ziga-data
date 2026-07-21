package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/store"
	"golang.org/x/oauth2"
)

const (
	oauthStateCookie = "ziga_oauth_state"
	oauthStateTTL    = 10 * time.Minute
	googleProvider   = "google"
)

// redirectApp sends the browser to a path in the SPA (used at the end of OAuth
// flows, which are top-level navigations rather than fetch calls).
func (s *Server) redirectApp(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, s.baseURL+path, http.StatusSeeOther)
}

// handleGoogleStart begins the OAuth flow: it stores an anti-forgery state in a
// short-lived cookie and redirects to Google's consent screen.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil || !s.oauth.Configured() {
		httpError(w, http.StatusNotFound, "Google sign-in is not configured")
		return
	}
	state, err := auth.RandomToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(oauthStateTTL), MaxAge: int(oauthStateTTL / time.Second),
	})
	http.Redirect(w, r, s.oauth.AuthCodeURL(state), http.StatusSeeOther)
}

// handleGoogleCallback completes the flow: it verifies state, exchanges the
// code, resolves/creates the user (linking to an existing account only when
// that account's email is already verified), stores the encrypted tokens, and
// starts a session. It always ends in a redirect into the SPA.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil || !s.oauth.Configured() {
		httpError(w, http.StatusNotFound, "Google sign-in is not configured")
		return
	}
	// Anti-forgery: the state in the query must match the cookie.
	stateCookie, err := r.Cookie(oauthStateCookie)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		s.redirectApp(w, r, "/login?oauth_error=state")
		return
	}
	// Clear the one-shot state cookie.
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectApp(w, r, "/login?oauth_error=denied")
		return
	}
	ctx := r.Context()
	tok, err := s.oauth.Exchange(ctx, code)
	if err != nil {
		s.log.Error("oauth exchange", "err", err)
		s.redirectApp(w, r, "/login?oauth_error=exchange")
		return
	}
	info, err := s.oauth.FetchUserInfo(ctx, tok)
	if err != nil {
		s.log.Error("oauth userinfo", "err", err)
		s.redirectApp(w, r, "/login?oauth_error=userinfo")
		return
	}

	uid, redirect, err := s.resolveGoogleUser(ctx, info)
	if err != nil {
		if errors.Is(err, errLinkUnverified) {
			s.redirectApp(w, r, "/login?oauth_error=verify_first")
			return
		}
		s.log.Error("oauth resolve user", "err", err)
		s.redirectApp(w, r, "/login?oauth_error=server")
		return
	}

	if err := s.storeGoogleTokens(ctx, uid, info.Sub, tok); err != nil {
		s.log.Error("store oauth tokens", "err", err)
		s.redirectApp(w, r, "/login?oauth_error=server")
		return
	}
	if err := s.startSession(w, r, uid); err != nil {
		s.log.Error("oauth start session", "err", err)
		s.redirectApp(w, r, "/login?oauth_error=server")
		return
	}
	s.redirectApp(w, r, redirect)
}

// errLinkUnverified signals that a Google sign-in matched an existing but
// unverified email/password account; per policy we refuse to auto-link.
var errLinkUnverified = errors.New("account exists but is unverified")

// resolveGoogleUser maps a Google identity to a user id, returning the path to
// redirect to afterward. Linking rules (decision: link only if verified):
//   - known Google sub  -> that user
//   - email matches a verified account -> link to it
//   - email matches an unverified account -> refuse (errLinkUnverified)
//   - no match -> create a new (Google-verified) account
func (s *Server) resolveGoogleUser(ctx context.Context, info *oauth.UserInfo) (int64, string, error) {
	if acct, err := s.store.GetOAuthAccountBySub(ctx, info.Sub); err == nil {
		return acct.UserID, "/", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", err
	}

	email := normalizeEmail(info.Email)
	existing, err := s.store.GetUserByEmail(ctx, email)
	switch {
	case err == nil && existing.Verified():
		return existing.ID, "/", nil
	case err == nil && !existing.Verified():
		return 0, "", errLinkUnverified
	case errors.Is(err, store.ErrNotFound):
		u, cerr := s.store.CreateUser(ctx, email, "")
		if cerr != nil {
			return 0, "", cerr
		}
		// Google asserts the email; trust it so the user isn't asked to verify
		// an address they signed in with.
		if info.EmailVerified {
			if verr := s.store.MarkEmailVerified(ctx, u.ID); verr != nil {
				return 0, "", verr
			}
		}
		// New Google users still need to connect a destination sheet.
		return u.ID, "/onboarding", nil
	default:
		return 0, "", err
	}
}

// storeGoogleTokens encrypts and persists the OAuth tokens. If Google omitted a
// refresh token on re-consent, the previously stored one is preserved.
func (s *Server) storeGoogleTokens(ctx context.Context, uid int64, sub string, tok *oauth2.Token) error {
	if s.box == nil {
		return errors.New("token encryption not configured")
	}
	accessEnc, err := s.box.SealString(tok.AccessToken)
	if err != nil {
		return err
	}
	var refreshEnc []byte
	if tok.RefreshToken != "" {
		if refreshEnc, err = s.box.SealString(tok.RefreshToken); err != nil {
			return err
		}
	} else if prior, perr := s.store.GetOAuthAccount(ctx, uid, googleProvider); perr == nil {
		refreshEnc = prior.RefreshTokenEnc
	}
	return s.store.UpsertOAuthAccount(ctx, &store.OAuthAccount{
		UserID:          uid,
		Provider:        googleProvider,
		GoogleSub:       sub,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		TokenExpiry:     tok.Expiry,
		Scopes:          strings.Join(s.oauth.Scopes(), " "),
	})
}

// handleGoogleDisconnect removes the current user's Google link and marks their
// sheet destination unwritable (its access came from this grant).
func (s *Server) handleGoogleDisconnect(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	if err := s.store.DeleteOAuthAccount(r.Context(), uid, googleProvider); err != nil {
		s.log.Error("disconnect google", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.MarkSheetBroken(r.Context(), uid); err != nil {
		s.log.Error("mark sheet broken on disconnect", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}
