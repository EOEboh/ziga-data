package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"testing/fstest"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/store"
)

// authTest is a tiny in-process client with a cookie jar, so it carries the
// session and CSRF cookies across requests like a browser.
type authTest struct {
	t       *testing.T
	s       *Server
	h       http.Handler
	mailbox *mail.FakeMailer
	cookies map[string]string
}

func newAuthTest(t *testing.T) *authTest {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		RatePerMin: 1000, SheetTab: "Leads",
		SessionSecret: "test-secret", AppBaseURL: "http://localhost:8080",
		Schema: config.Schema{
			Fields:  []config.Field{{Name: "need"}},
			Columns: []string{"need"},
		},
	}
	fake := &mail.FakeMailer{}
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeExtractor{result: goodResult()}, st, &fakeWriter{}, fake)
	a := &authTest{
		t: t, s: s, mailbox: fake, cookies: map[string]string{},
		h: s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}),
	}
	// Prime the CSRF cookie.
	a.do("GET", "/api/me", nil, true)
	return a
}

// do issues a request carrying the jar's cookies (and, when csrf is true, the
// matching X-CSRF-Token header), then absorbs any Set-Cookie into the jar.
func (a *authTest) do(method, path string, body any, csrf bool) *httptest.ResponseRecorder {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", a.cookies[csrfCookie])
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(a.cookies, c.Name)
		} else {
			a.cookies[c.Name] = c.Value
		}
	}
	return rec
}

var linkTokenRE = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

func (a *authTest) lastEmailToken() string {
	a.t.Helper()
	msg, ok := a.mailbox.Last()
	if !ok {
		a.t.Fatal("no email was sent")
	}
	m := linkTokenRE.FindStringSubmatch(msg.Text)
	if m == nil {
		a.t.Fatalf("no token in email body: %q", msg.Text)
	}
	return m[1]
}

func TestSignupVerifyGateAndLogin(t *testing.T) {
	a := newAuthTest(t)

	// Signup creates an (unverified) account and sends a verification email.
	if rec := a.do("POST", "/api/auth/signup", map[string]string{"email": "jane@x.com", "password": "hunter2hunter"}, true); rec.Code != http.StatusCreated {
		t.Fatalf("signup code=%d", rec.Code)
	}
	if len(a.mailbox.Sent) != 1 {
		t.Fatalf("expected 1 verification email, got %d", len(a.mailbox.Sent))
	}

	// Login is refused until the email is verified.
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "jane@x.com", "password": "hunter2hunter"}, true); rec.Code != http.StatusForbidden {
		t.Fatalf("login before verify: code=%d, want 403", rec.Code)
	}

	// Follow the verification link.
	token := a.lastEmailToken()
	if rec := a.do("GET", "/api/auth/verify?token="+token, nil, false); rec.Code != http.StatusSeeOther {
		t.Fatalf("verify code=%d, want 303", rec.Code)
	}

	// Now login succeeds and a protected route is reachable.
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "jane@x.com", "password": "hunter2hunter"}, true); rec.Code != 200 {
		t.Fatalf("login after verify: code=%d, want 200", rec.Code)
	}
	if a.cookies[sessionCookie] == "" {
		t.Fatal("login must set a session cookie")
	}
	if rec := a.do("GET", "/api/queue", nil, false); rec.Code != 200 {
		t.Fatalf("protected route after login: code=%d, want 200", rec.Code)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	a := newAuthTest(t)
	a.do("POST", "/api/auth/signup", map[string]string{"email": "j@x.com", "password": "hunter2hunter"}, true)
	// Unknown email and wrong password both return the same generic 401.
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "nobody@x.com", "password": "whatever!"}, true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown email: code=%d, want 401", rec.Code)
	}
	token := a.lastEmailToken()
	a.do("GET", "/api/auth/verify?token="+token, nil, false)
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "j@x.com", "password": "wrongpass!"}, true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: code=%d, want 401", rec.Code)
	}
}

func TestDuplicateSignupConflicts(t *testing.T) {
	a := newAuthTest(t)
	a.do("POST", "/api/auth/signup", map[string]string{"email": "dup@x.com", "password": "hunter2hunter"}, true)
	if rec := a.do("POST", "/api/auth/signup", map[string]string{"email": "dup@x.com", "password": "hunter2hunter"}, true); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate signup: code=%d, want 409", rec.Code)
	}
}

func TestCSRFRejected(t *testing.T) {
	a := newAuthTest(t)
	// Missing header.
	if rec := a.do("POST", "/api/auth/signup", map[string]string{"email": "c@x.com", "password": "hunter2hunter"}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: code=%d, want 403", rec.Code)
	}
	// Mismatched header.
	req := httptest.NewRequest("POST", "/api/auth/signup", bytes.NewReader([]byte(`{"email":"c@x.com","password":"hunter2hunter"}`)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	req.Header.Set("X-CSRF-Token", "not-the-cookie-value")
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mismatched CSRF: code=%d, want 403", rec.Code)
	}
}

func TestSessionExpiryAndLogout(t *testing.T) {
	a := newAuthTest(t)
	ctx := context.Background()
	// Seed a verified user directly and mint an already-expired session.
	u, _ := a.s.store.CreateUser(ctx, "exp@x.com", "")
	a.s.store.MarkEmailVerified(ctx, u.ID)
	token, _ := auth.RandomToken()
	a.s.store.CreateSession(ctx, auth.HashToken(token), u.ID, time.Now().Add(-time.Minute))
	a.cookies[sessionCookie] = token
	if rec := a.do("GET", "/api/queue", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: code=%d, want 401", rec.Code)
	}

	// A live session, then logout, then the route is protected again.
	live, _ := auth.RandomToken()
	a.s.store.CreateSession(ctx, auth.HashToken(live), u.ID, time.Now().Add(time.Hour))
	a.cookies[sessionCookie] = live
	if rec := a.do("GET", "/api/queue", nil, false); rec.Code != 200 {
		t.Fatalf("live session: code=%d, want 200", rec.Code)
	}
	if rec := a.do("POST", "/api/auth/logout", nil, true); rec.Code != 200 {
		t.Fatalf("logout: code=%d", rec.Code)
	}
	if a.cookies[sessionCookie] != "" {
		t.Fatal("logout must clear the session cookie")
	}
}

func TestPasswordResetRoundTrip(t *testing.T) {
	a := newAuthTest(t)
	a.do("POST", "/api/auth/signup", map[string]string{"email": "r@x.com", "password": "originalpass"}, true)

	// Forgot → reset email.
	if rec := a.do("POST", "/api/auth/password/forgot", map[string]string{"email": "r@x.com"}, true); rec.Code != 200 {
		t.Fatalf("forgot: code=%d", rec.Code)
	}
	resetToken := a.lastEmailToken() // most recent email is the reset

	// Reset sets a new password (and verifies the account).
	if rec := a.do("POST", "/api/auth/password/reset", map[string]string{"token": resetToken, "password": "brandnewpass"}, true); rec.Code != 200 {
		t.Fatalf("reset: code=%d", rec.Code)
	}
	// Reusing the token fails (single use).
	if rec := a.do("POST", "/api/auth/password/reset", map[string]string{"token": resetToken, "password": "another1pass"}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("reused reset token: code=%d, want 400", rec.Code)
	}
	// Login with the new password works; the old one does not.
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "r@x.com", "password": "brandnewpass"}, true); rec.Code != 200 {
		t.Fatalf("login with new password: code=%d, want 200", rec.Code)
	}
	if rec := a.do("POST", "/api/auth/login", map[string]string{"email": "r@x.com", "password": "originalpass"}, true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password: code=%d, want 401", rec.Code)
	}
}

func TestForgotUnknownEmailDoesNotLeak(t *testing.T) {
	a := newAuthTest(t)
	if rec := a.do("POST", "/api/auth/password/forgot", map[string]string{"email": "ghost@x.com"}, true); rec.Code != 200 {
		t.Fatalf("forgot unknown: code=%d, want 200 (no enumeration)", rec.Code)
	}
	if len(a.mailbox.Sent) != 0 {
		t.Fatal("no email should be sent for an unknown address")
	}
}

func TestLoginBruteForceRateLimited(t *testing.T) {
	a := newAuthTest(t)
	a.do("POST", "/api/auth/signup", map[string]string{"email": "b@x.com", "password": "hunter2hunter"}, true)
	// The login limiter has burst 5; after that, requests from the same IP 429.
	sawLimit := false
	for i := 0; i < 8; i++ {
		rec := a.do("POST", "/api/auth/login", map[string]string{"email": "b@x.com", "password": "wrongpass!"}, true)
		if rec.Code == http.StatusTooManyRequests {
			sawLimit = true
			break
		}
	}
	if !sawLimit {
		t.Fatal("repeated logins from one IP must eventually be rate-limited (429)")
	}
}

func TestMeReflectsSession(t *testing.T) {
	a := newAuthTest(t)
	rec := a.do("GET", "/api/me", nil, false)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != false {
		t.Fatalf("unauthenticated /api/me: %v", body)
	}

	a.do("POST", "/api/auth/signup", map[string]string{"email": "m@x.com", "password": "hunter2hunter"}, true)
	token := a.lastEmailToken()
	a.do("GET", "/api/auth/verify?token="+token, nil, false)
	a.do("POST", "/api/auth/login", map[string]string{"email": "m@x.com", "password": "hunter2hunter"}, true)

	rec = a.do("GET", "/api/me", nil, false)
	body = map[string]any{}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != true {
		t.Fatalf("authenticated /api/me: %v", body)
	}
	user := body["user"].(map[string]any)
	if user["email"] != "m@x.com" || user["email_verified"] != true {
		t.Fatalf("me.user = %v", user)
	}
}
