package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/secretbox"
	"github.com/EOEboh/ziga-data/internal/store"
)

// fakeGoogle stands in for Google's token + userinfo endpoints.
type fakeGoogle struct {
	server *httptest.Server
	info   oauth.UserInfo
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	fg := &fakeGoogle{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"acc-1","refresh_token":"ref-1","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fg.info)
	})
	fg.server = httptest.NewServer(mux)
	t.Cleanup(fg.server.Close)
	return fg
}

// newGoogleTest builds an authTest whose server has Google OAuth configured and
// pointed at a fake Google, with token encryption enabled.
func newGoogleTest(t *testing.T) (*authTest, *fakeGoogle) {
	t.Helper()
	fg := newFakeGoogle(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	cfg := &config.Config{
		RatePerMin: 1000, SheetTab: "Leads",
		SessionSecret: "test-secret", AppBaseURL: "http://localhost:8080",
		GoogleOAuthClientID: "client-id", GoogleOAuthClientSecret: "secret",
		Schema: config.Schema{Fields: []config.Field{{Name: "need"}}, Columns: []string{"need"}},
	}
	oc := oauth.NewConfig(cfg.GoogleOAuthClientID, cfg.GoogleOAuthClientSecret, "http://localhost:8080/api/auth/google/callback")
	oc.SetEndpoints(fg.server.URL+"/authorize", fg.server.URL+"/token", fg.server.URL+"/userinfo")
	fake := &mail.FakeMailer{}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeExtractor{result: goodResult()}, st, &fakeWriter{}, fake, oc, box)
	a := &authTest{
		t: t, s: s, mailbox: fake, cookies: map[string]string{},
		h: s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}),
	}
	a.do("GET", "/api/me", nil, true)
	return a, fg
}

// runOAuthCallback performs the start -> callback round-trip and returns the
// callback response recorder.
func (a *authTest) runOAuthCallback(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	start := a.do("GET", "/api/auth/google/start", nil, false)
	if start.Code != http.StatusSeeOther {
		t.Fatalf("start code=%d, want 303", start.Code)
	}
	state := a.cookies[oauthStateCookie]
	if state == "" {
		t.Fatal("start must set an oauth state cookie")
	}
	return a.do("GET", "/api/auth/google/callback?code=abc&state="+state, nil, false)
}

func TestGoogleCallbackCreatesVerifiedUser(t *testing.T) {
	a, fg := newGoogleTest(t)
	fg.info = oauth.UserInfo{Sub: "google-1", Email: "new@x.com", EmailVerified: true}

	rec := a.runOAuthCallback(t)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback code=%d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "http://localhost:8080/onboarding" {
		t.Fatalf("new user should land on onboarding, got %q", loc)
	}
	if a.cookies[sessionCookie] == "" {
		t.Fatal("callback must start a session")
	}

	ctx := context.Background()
	u, err := a.s.store.GetUserByEmail(ctx, "new@x.com")
	if err != nil || !u.Verified() {
		t.Fatalf("google user must exist and be verified: %+v err=%v", u, err)
	}
	// Tokens are stored encrypted; ciphertext must not contain the plaintext.
	acct, err := a.s.store.GetOAuthAccountBySub(ctx, "google-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(acct.AccessTokenEnc) == "acc-1" || string(acct.RefreshTokenEnc) == "ref-1" {
		t.Fatal("tokens must be encrypted at rest, not stored in plaintext")
	}
	access, _ := a.s.box.OpenString(acct.AccessTokenEnc)
	refresh, _ := a.s.box.OpenString(acct.RefreshTokenEnc)
	if access != "acc-1" || refresh != "ref-1" {
		t.Fatalf("decrypted tokens = %q / %q", access, refresh)
	}
}

func TestGoogleCallbackLinksVerifiedAccount(t *testing.T) {
	a, fg := newGoogleTest(t)
	ctx := context.Background()
	// Pre-existing verified email+password account with the same email.
	u, _ := a.s.store.CreateUser(ctx, "both@x.com", "pwhash")
	a.s.store.MarkEmailVerified(ctx, u.ID)

	fg.info = oauth.UserInfo{Sub: "google-2", Email: "both@x.com", EmailVerified: true}
	rec := a.runOAuthCallback(t)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback code=%d", rec.Code)
	}
	// Linked to the SAME user, not a new one.
	acct, err := a.s.store.GetOAuthAccountBySub(ctx, "google-2")
	if err != nil || acct.UserID != u.ID {
		t.Fatalf("google identity must link to the existing verified user: %+v err=%v", acct, err)
	}
}

func TestGoogleCallbackRefusesUnverifiedAccount(t *testing.T) {
	a, fg := newGoogleTest(t)
	ctx := context.Background()
	// Pre-existing UNVERIFIED email+password account.
	a.s.store.CreateUser(ctx, "pending@x.com", "pwhash")

	fg.info = oauth.UserInfo{Sub: "google-3", Email: "pending@x.com", EmailVerified: true}
	rec := a.runOAuthCallback(t)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback code=%d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "http://localhost:8080/login?oauth_error=verify_first" {
		t.Fatalf("unverified match must be refused, got %q", loc)
	}
	if a.cookies[sessionCookie] != "" {
		t.Fatal("no session may be started when linking is refused")
	}
	if _, err := a.s.store.GetOAuthAccountBySub(ctx, "google-3"); err == nil {
		t.Fatal("no oauth account should be created for a refused link")
	}
}

func TestGoogleCallbackRejectsBadState(t *testing.T) {
	a, _ := newGoogleTest(t)
	a.do("GET", "/api/auth/google/start", nil, false)
	// Wrong state value (CSRF / forgery attempt).
	rec := a.do("GET", "/api/auth/google/callback?code=abc&state=not-the-state", nil, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "http://localhost:8080/login?oauth_error=state" {
		t.Fatalf("bad state must be rejected, code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestGoogleDisconnect(t *testing.T) {
	a, fg := newGoogleTest(t)
	fg.info = oauth.UserInfo{Sub: "google-4", Email: "d@x.com", EmailVerified: true}
	a.runOAuthCallback(t)

	ctx := context.Background()
	u, _ := a.s.store.GetUserByEmail(ctx, "d@x.com")
	if rec := a.do("POST", "/api/auth/google/disconnect", nil, true); rec.Code != 200 {
		t.Fatalf("disconnect code=%d", rec.Code)
	}
	if _, err := a.s.store.GetOAuthAccount(ctx, u.ID, "google"); err == nil {
		t.Fatal("oauth account must be removed on disconnect")
	}
}

func TestGoogleRoutes404WhenUnconfigured(t *testing.T) {
	a := newAuthTest(t) // oauth not configured
	if rec := a.do("GET", "/api/auth/google/start", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("start without oauth configured: code=%d, want 404", rec.Code)
	}
}
