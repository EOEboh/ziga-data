package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/notionauth"
	"github.com/EOEboh/ziga-data/internal/store"
)

const (
	notionProvider    = "notion"
	notionStateCookie = "ziga_notion_state"
)

// notionEnabled reports whether Notion can be offered as a destination: the
// OAuth client is configured and token encryption is available, so a granted
// token can be stored safely.
func (s *Server) notionEnabled() bool {
	return s.notionAuth != nil && s.notionAuth.Configured() && s.box != nil
}

// requireNotion writes the 404 used by every Notion route when Notion is not
// configured, and reports whether the caller may proceed.
func (s *Server) requireNotion(w http.ResponseWriter) bool {
	if !s.notionEnabled() {
		httpError(w, http.StatusNotFound, "Notion is not configured")
		return false
	}
	return true
}

// handleNotionStart begins the Notion OAuth flow with an anti-forgery state
// cookie, mirroring the Google flow.
//
// Unlike Google, this is not a sign-in path — Notion is only ever a
// destination — so the route is session-protected and the user is already
// known when the callback returns.
func (s *Server) handleNotionStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	state, err := auth.RandomToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: notionStateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(oauthStateTTL), MaxAge: int(oauthStateTTL / time.Second),
	})
	http.Redirect(w, r, s.notionAuth.AuthCodeURL(state), http.StatusSeeOther)
}

// handleNotionCallback completes the flow: it verifies state, exchanges the
// code, and stores the encrypted workspace token. It does not yet set a
// destination — the user still has to choose or create a database — so it ends
// by redirecting into the Notion setup step of the SPA.
func (s *Server) handleNotionCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	// Anti-forgery: the state in the query must match the cookie.
	stateCookie, err := r.Cookie(notionStateCookie)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		s.redirectApp(w, r, "/onboarding/notion?notion_error=state")
		return
	}
	// Clear the one-shot state cookie.
	http.SetCookie(w, &http.Cookie{Name: notionStateCookie, Value: "", Path: "/", MaxAge: -1})

	// The user may have declined on Notion's consent screen.
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectApp(w, r, "/onboarding/notion?notion_error=denied")
		return
	}

	ctx := r.Context()
	grant, err := s.notionAuth.Exchange(ctx, code)
	if err != nil {
		s.log.Error("notion exchange", "err", err)
		s.redirectApp(w, r, "/onboarding/notion?notion_error=exchange")
		return
	}
	if err := s.storeNotionToken(ctx, userID(r), grant); err != nil {
		s.log.Error("store notion token", "err", err)
		s.redirectApp(w, r, "/onboarding/notion?notion_error=server")
		return
	}
	s.log.Info("notion workspace connected", "workspace", grant.WorkspaceName)
	s.redirectApp(w, r, "/onboarding/notion")
}

// storeNotionToken encrypts and persists the workspace access token, reusing
// the same secretbox that protects Google's tokens — a Notion token is never
// written to disk in plaintext.
//
// Notion access tokens are long-lived and a refresh token is only issued to
// integrations using token rotation, so both the refresh token and the expiry
// are optional. A reconnect for the same workspace upserts onto the existing
// row (bot id is the stable per-install identity) and clears any broken flag.
func (s *Server) storeNotionToken(ctx context.Context, uid int64, grant *notionauth.Grant) error {
	if s.box == nil {
		return errors.New("token encryption not configured")
	}
	accessEnc, err := s.box.SealString(grant.AccessToken)
	if err != nil {
		return err
	}
	var refreshEnc []byte
	if grant.RefreshToken != "" {
		if refreshEnc, err = s.box.SealString(grant.RefreshToken); err != nil {
			return err
		}
	}
	return s.store.UpsertOAuthAccount(ctx, &store.OAuthAccount{
		UserID:          uid,
		Provider:        notionProvider,
		ProviderSub:     grant.BotID,
		AccessTokenEnc:  accessEnc,
		RefreshTokenEnc: refreshEnc,
		Scopes:          "notion:granted-resources",
	})
}

// notionAccessToken decrypts the user's stored Notion token. A missing or
// broken link is errReconnect, which handlers surface as a reconnect prompt.
func (s *Server) notionAccessToken(ctx context.Context, uid int64) (string, error) {
	acct, err := s.store.GetOAuthAccount(ctx, uid, notionProvider)
	if errors.Is(err, store.ErrNotFound) {
		return "", errReconnect
	}
	if err != nil {
		return "", err
	}
	if acct.Broken() {
		return "", errReconnect
	}
	return s.box.OpenString(acct.AccessTokenEnc)
}

// markNotionBroken flags the Notion link, and the destination when it is the
// Notion one, so the UI prompts a reconnect instead of failing writes
// silently. This is the revoked-access / uninstalled-integration path.
func (s *Server) markNotionBroken(ctx context.Context, uid int64) {
	if err := s.store.MarkOAuthBroken(ctx, uid, notionProvider); err != nil {
		s.log.Error("mark notion broken", "err", err)
	}
	if dest, err := s.store.GetDestination(ctx, uid); err == nil &&
		destination.Type(dest.Type) == destination.TypeNotion {
		if err := s.store.MarkDestinationBroken(ctx, uid); err != nil {
			s.log.Error("mark destination broken", "err", err)
		}
	}
}

// handleNotionDisconnect drops the Notion link and its stored token. A Notion
// destination loses its access with it, so it is marked broken; a Google Sheet
// destination is untouched.
func (s *Server) handleNotionDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireNotion(w) {
		return
	}
	ctx := r.Context()
	uid := userID(r)
	if err := s.store.DeleteOAuthAccount(ctx, uid, notionProvider); err != nil {
		s.log.Error("disconnect notion", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if dest, err := s.store.GetDestination(ctx, uid); err == nil &&
		destination.Type(dest.Type) == destination.TypeNotion {
		if err := s.store.MarkDestinationBroken(ctx, uid); err != nil {
			s.log.Error("mark destination broken on notion disconnect", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// notionConnected reports whether the user has a healthy Notion link.
func (s *Server) notionConnected(ctx context.Context, uid int64) bool {
	acct, err := s.store.GetOAuthAccount(ctx, uid, notionProvider)
	return err == nil && !acct.Broken()
}
