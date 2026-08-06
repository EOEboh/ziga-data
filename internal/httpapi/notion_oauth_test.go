package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/notionauth"
	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/secretbox"
	"github.com/EOEboh/ziga-data/internal/store"
)

// fakeNotionOAuth stands in for Notion's token endpoint. It records the
// Authorization header so the test can assert Notion's required HTTP Basic
// client authentication actually happens.
type fakeNotionOAuth struct {
	server *httptest.Server
	// token is the JSON body returned from the token endpoint.
	token map[string]any
	// status, when non-zero, is returned instead of the token body.
	status int

	gotAuthHeader string
	gotBody       map[string]any
}

func newFakeNotionOAuth(t *testing.T) *fakeNotionOAuth {
	t.Helper()
	// A distinct bot id per test: Notion issues one per workspace install, and
	// the client's rate limiter is keyed by it, so sharing one across tests
	// would serialize them all through a single 3-per-second budget.
	f := &fakeNotionOAuth{
		token: map[string]any{
			"access_token":   "ntn-secret-token",
			"token_type":     "bearer",
			"bot_id":         "bot-" + t.Name(),
			"workspace_id":   "ws-1",
			"workspace_name": "Acme HQ",
			"workspace_icon": "https://example.invalid/icon.png",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuthHeader = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		f.gotBody = map[string]any{}
		// The oauth2 package posts form-encoded credentials.
		for _, pair := range strings.Split(string(body), "&") {
			k, v, _ := strings.Cut(pair, "=")
			f.gotBody[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 {
			w.WriteHeader(f.status)
			w.Write([]byte(`{"object":"error","status":401,"code":"unauthorized","message":"invalid client"}`))
			return
		}
		json.NewEncoder(w).Encode(f.token)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// newNotionTest builds a server with Notion configured against the fake, plus
// a logged-in user. Google stays configured too, since the two providers
// coexist.
func newNotionTest(t *testing.T) (*authTest, *fakeNotionOAuth, int64) {
	t.Helper()
	fn := newFakeNotionOAuth(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "notion.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, _ := secretbox.GenerateKey()
	box, _ := secretbox.New(key)
	cfg := &config.Config{
		RatePerMin: 1000, SheetTab: "Leads",
		SessionSecret: "test-secret", AppBaseURL: "http://localhost:8080",
		NotionOAuthClientID:     "notion-client",
		NotionOAuthClientSecret: "notion-secret",
		NotionOAuthRedirectURL:  "http://localhost:8080/api/notion/callback",
		NotionVersion:           config.DefaultNotionVersion,
		// The real schema: the mapping flow is only meaningful across the full
		// field set, and "flags" is the synthetic column the writer adds.
		Schema: config.Schema{
			RequiredFields: []string{"contact", "need"},
			Fields: []config.Field{
				{Name: "name"}, {Name: "contact"}, {Name: "source"},
				{Name: "need"}, {Name: "date"}, {Name: "notes"},
			},
			Columns: []string{"date", "name", "contact", "source", "need", "notes", "flags"},
		},
	}
	nc := notionauth.NewConfig(cfg.NotionOAuthClientID, cfg.NotionOAuthClientSecret, cfg.NotionOAuthRedirectURL)
	nc.SetEndpoints(fn.server.URL+"/oauth/authorize", fn.server.URL+"/oauth/token")

	fake := &mail.FakeMailer{}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeExtractor{result: goodResult()},
		st, &fakeWriter{}, fake, oauth.NewConfig("", "", ""), nc, box)
	a := &authTest{
		t: t, s: s, mailbox: fake, cookies: map[string]string{},
		h: s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}),
	}
	a.do("GET", "/api/me", nil, true)

	uid := mustVerifiedUser(t, a, "notion-user@x.com")
	a.cookies[sessionCookie] = mustSession(t, a, uid)
	return a, fn, uid
}

// runNotionConnect performs the start -> callback round-trip.
func runNotionConnect(t *testing.T, a *authTest) *httptest.ResponseRecorder {
	t.Helper()
	start := a.do("GET", "/api/notion/start", nil, false)
	if start.Code != http.StatusSeeOther {
		t.Fatalf("notion start code=%d, want 303", start.Code)
	}
	state := a.cookies[notionStateCookie]
	if state == "" {
		t.Fatal("start must set a notion state cookie")
	}
	return a.do("GET", "/api/notion/callback?code=abc&state="+state, nil, false)
}

func TestNotionConnectStoresEncryptedToken(t *testing.T) {
	a, fn, uid := newNotionTest(t)
	ctx := context.Background()

	if rec := runNotionConnect(t, a); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Notion requires HTTP Basic client authentication on the token endpoint.
	const prefix = "Basic "
	if !strings.HasPrefix(fn.gotAuthHeader, prefix) {
		t.Fatalf("token request Authorization = %q, want HTTP Basic", fn.gotAuthHeader)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fn.gotAuthHeader, prefix))
	if err != nil {
		t.Fatalf("basic credentials not base64: %v", err)
	}
	if string(raw) != "notion-client:notion-secret" {
		t.Fatalf("basic credentials = %q, want client:secret", raw)
	}

	acct, err := a.s.store.GetOAuthAccount(ctx, uid, notionProvider)
	if err != nil {
		t.Fatalf("notion link not stored: %v", err)
	}
	if acct.ProviderSub != "bot-"+t.Name() {
		t.Fatalf("provider_sub = %q, want the bot id", acct.ProviderSub)
	}

	// The token is ciphertext at rest and round-trips through the same
	// secretbox that protects Google's tokens.
	if len(acct.AccessTokenEnc) == 0 {
		t.Fatal("no access token stored")
	}
	if strings.Contains(string(acct.AccessTokenEnc), "ntn-secret-token") {
		t.Fatal("access token is stored in plaintext")
	}
	plain, err := a.s.box.OpenString(acct.AccessTokenEnc)
	if err != nil || plain != "ntn-secret-token" {
		t.Fatalf("token did not round-trip: %q err=%v", plain, err)
	}
}

func TestNotionCallbackRejectsBadState(t *testing.T) {
	a, _, uid := newNotionTest(t)
	// A callback whose state does not match the cookie must not store anything.
	a.do("GET", "/api/notion/start", nil, false)
	rec := a.do("GET", "/api/notion/callback?code=abc&state=not-the-cookie", nil, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d, want a redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notion_error=state") {
		t.Fatalf("Location = %q, want a state error", loc)
	}
	if _, err := a.s.store.GetOAuthAccount(context.Background(), uid, notionProvider); err == nil {
		t.Fatal("a forged callback must not create a notion link")
	}
}

func TestNotionCallbackWithoutCodeIsDenied(t *testing.T) {
	a, _, uid := newNotionTest(t)
	a.do("GET", "/api/notion/start", nil, false)
	state := a.cookies[notionStateCookie]
	rec := a.do("GET", "/api/notion/callback?state="+state, nil, false)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notion_error=denied") {
		t.Fatalf("Location = %q, want a denied error", loc)
	}
	if _, err := a.s.store.GetOAuthAccount(context.Background(), uid, notionProvider); err == nil {
		t.Fatal("declining consent must not create a notion link")
	}
}

func TestNotionExchangeFailureSurfaces(t *testing.T) {
	a, fn, uid := newNotionTest(t)
	fn.status = http.StatusUnauthorized
	rec := runNotionConnect(t, a)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "notion_error=exchange") {
		t.Fatalf("Location = %q, want an exchange error", loc)
	}
	if _, err := a.s.store.GetOAuthAccount(context.Background(), uid, notionProvider); err == nil {
		t.Fatal("a failed exchange must not create a notion link")
	}
}

// Reconnecting the same workspace updates the existing link and clears the
// broken flag, rather than failing on the unique provider_sub constraint.
func TestNotionReconnectClearsBroken(t *testing.T) {
	a, _, uid := newNotionTest(t)
	ctx := context.Background()
	runNotionConnect(t, a)

	if err := a.s.store.MarkOAuthBroken(ctx, uid, notionProvider); err != nil {
		t.Fatal(err)
	}
	if a.s.notionConnected(ctx, uid) {
		t.Fatal("a broken link must not read as connected")
	}

	if rec := runNotionConnect(t, a); rec.Code != http.StatusSeeOther {
		t.Fatalf("reconnect code=%d", rec.Code)
	}
	if !a.s.notionConnected(ctx, uid) {
		t.Fatal("reconnecting must clear the broken flag")
	}
}

func TestNotionDisconnectDropsLink(t *testing.T) {
	a, _, uid := newNotionTest(t)
	ctx := context.Background()
	runNotionConnect(t, a)

	if rec := a.do("POST", "/api/notion/disconnect", map[string]string{}, true); rec.Code != 200 {
		t.Fatalf("disconnect code=%d", rec.Code)
	}
	if _, err := a.s.store.GetOAuthAccount(ctx, uid, notionProvider); err == nil {
		t.Fatal("disconnect must remove the stored token")
	}
}

// With Notion unconfigured the routes must 404 rather than half-work.
func TestNotionRoutesAbsentWhenUnconfigured(t *testing.T) {
	a := newAuthTest(t)
	uid := mustVerifiedUser(t, a, "no-notion@x.com")
	a.cookies[sessionCookie] = mustSession(t, a, uid)

	for _, route := range []struct {
		method, path string
	}{
		{"GET", "/api/notion/start"},
		{"GET", "/api/notion/callback?code=x&state=y"},
		{"POST", "/api/notion/disconnect"},
	} {
		rec := a.do(route.method, route.path, map[string]string{}, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code=%d, want 404 when Notion is unconfigured",
				route.method, route.path, rec.Code)
		}
	}
}
