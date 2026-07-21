package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAuthCodeURLRequestsOfflineDriveFile(t *testing.T) {
	c := NewConfig("client-id", "secret", "https://app/callback")
	url := c.AuthCodeURL("state123")
	for _, want := range []string{"state=state123", "access_type=offline", "prompt=consent", "drive.file"} {
		if !strings.Contains(url, want) {
			t.Errorf("auth URL missing %q: %s", want, url)
		}
	}
	// It must NOT request the broad spreadsheets scope.
	if strings.Contains(url, "auth/spreadsheets") {
		t.Errorf("auth URL must not request the broad spreadsheets scope: %s", url)
	}
}

func TestFetchUserInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"sub":"google-abc","email":"jane@x.com","email_verified":true}`))
	}))
	defer srv.Close()

	c := NewConfig("id", "secret", "https://app/callback")
	c.SetEndpoints("", "", srv.URL)
	info, err := c.FetchUserInfo(context.Background(), &oauth2.Token{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Sub != "google-abc" || info.Email != "jane@x.com" || !info.EmailVerified {
		t.Fatalf("userinfo = %+v", info)
	}
}

func TestTokenSourceRefreshNotifies(t *testing.T) {
	var refreshed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fresh-access","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	c := NewConfig("id", "secret", "https://app/callback")
	c.SetEndpoints("", srv.URL, "")
	expired := &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "refresh-tok",
		Expiry:       time.Now().Add(-time.Hour),
	}
	var got *oauth2.Token
	ts := c.TokenSource(context.Background(), expired, func(tok *oauth2.Token) {
		refreshed++
		got = tok
	})
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fresh-access" {
		t.Fatalf("access token = %q, want fresh-access", tok.AccessToken)
	}
	if refreshed != 1 || got == nil || got.AccessToken != "fresh-access" {
		t.Fatalf("onRefresh not called with the new token: n=%d got=%v", refreshed, got)
	}
}

func TestTokenSourceRefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewConfig("id", "secret", "https://app/callback")
	c.SetEndpoints("", srv.URL, "")
	expired := &oauth2.Token{AccessToken: "stale", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)}
	ts := c.TokenSource(context.Background(), expired, nil)
	if _, err := ts.Token(); err == nil {
		t.Fatal("a revoked refresh token must surface as an error from Token()")
	}
}
