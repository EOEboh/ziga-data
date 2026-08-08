package notionauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testConfig(t *testing.T, handler http.HandlerFunc) (*Config, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewConfig("client-abc", "secret-xyz", "http://localhost:8080/api/notion/callback")
	c.SetEndpoints(srv.URL+"/oauth/authorize", srv.URL+"/oauth/token")
	return c, srv
}

// Notion's public-integration flow requires owner=user on the consent URL and
// has no scope parameter: what the integration may reach is chosen by the user
// resource by resource, not requested up front.
func TestAuthCodeURLShape(t *testing.T) {
	c := NewConfig("client-abc", "secret-xyz", "http://localhost:8080/api/notion/callback")
	u, err := url.Parse(c.AuthCodeURL("state-123"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	for _, tc := range []struct{ key, want string }{
		{"client_id", "client-abc"},
		{"response_type", "code"},
		{"owner", "user"},
		{"state", "state-123"},
		{"redirect_uri", "http://localhost:8080/api/notion/callback"},
	} {
		if got := q.Get(tc.key); got != tc.want {
			t.Errorf("consent URL %s = %q, want %q", tc.key, got, tc.want)
		}
	}
	// Requesting a scope would be asking for blanket access, which is exactly
	// what this integration must not do.
	if q.Has("scope") {
		t.Errorf("consent URL must not request scopes, got %q", q.Get("scope"))
	}
	if u.Host != "api.notion.com" || u.Path != "/v1/oauth/authorize" {
		t.Errorf("consent URL = %s, want Notion's authorize endpoint", u)
	}
}

func TestConfigured(t *testing.T) {
	if NewConfig("", "", "").Configured() {
		t.Error("empty credentials must not report as configured")
	}
	if NewConfig("id", "", "").Configured() {
		t.Error("a missing secret must not report as configured")
	}
	if !NewConfig("id", "secret", "url").Configured() {
		t.Error("full credentials must report as configured")
	}
}

// Notion authenticates the token request with HTTP Basic, not credentials in
// the body, and returns the workspace identity alongside the token.
func TestExchangeUsesBasicAuthAndReturnsWorkspace(t *testing.T) {
	var gotAuth, gotBody string
	c, _ := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":   "ntn-abc",
			"token_type":     "bearer",
			"bot_id":         "bot-1",
			"workspace_id":   "ws-1",
			"workspace_name": "Acme HQ",
			"workspace_icon": "https://example.invalid/i.png",
		})
	})

	grant, err := c.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if grant.AccessToken != "ntn-abc" || grant.BotID != "bot-1" {
		t.Fatalf("grant = %+v", grant)
	}
	if grant.WorkspaceID != "ws-1" || grant.WorkspaceName != "Acme HQ" {
		t.Fatalf("workspace identity not read off the token response: %+v", grant)
	}
	// Notion's token endpoint returns no refresh token unless the integration
	// uses token rotation, so an absent one is normal rather than an error.
	if grant.RefreshToken != "" {
		t.Fatalf("RefreshToken = %q, want empty", grant.RefreshToken)
	}

	const prefix = "Basic "
	if !strings.HasPrefix(gotAuth, prefix) {
		t.Fatalf("Authorization = %q, want HTTP Basic", gotAuth)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(gotAuth, prefix))
	if err != nil || string(raw) != "client-abc:secret-xyz" {
		t.Fatalf("basic credentials = %q err=%v", raw, err)
	}
	// The secret must travel in the header, never in the body.
	if strings.Contains(gotBody, "secret-xyz") {
		t.Fatalf("client secret leaked into the request body: %q", gotBody)
	}
	// Notion requires redirect_uri in the exchange when the authorize URL
	// carried one (this client always sends it), and rejects a malformed body.
	// The config sets it and Exchange passes it again explicitly, so pin that
	// this collapses to exactly one parameter rather than duplicating.
	if n := strings.Count(gotBody, "redirect_uri="); n != 1 {
		t.Fatalf("body has %d redirect_uri params, want exactly 1: %q", n, gotBody)
	}
}

// A response without the fields the app depends on is an error, not a
// half-connected workspace that fails later.
func TestExchangeRejectsIncompleteResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"no bot id", map[string]any{
			"access_token": "ntn-abc", "token_type": "bearer",
		}, "bot_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(tc.body)
			})
			_, err := c.Exchange(context.Background(), "code")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestExchangeSurfacesProviderError(t *testing.T) {
	c, _ := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_client"}`))
	})
	if _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("a rejected exchange must return an error")
	}
}

// Token rotation is opt-in; when Notion does return a refresh token it must be
// carried through so it can be stored.
func TestExchangeKeepsRefreshTokenWhenPresent(t *testing.T) {
	c, _ := testConfig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ntn-abc", "refresh_token": "ntn-refresh",
			"token_type": "bearer", "bot_id": "bot-1",
		})
	})
	grant, err := c.Exchange(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if grant.RefreshToken != "ntn-refresh" {
		t.Fatalf("RefreshToken = %q, want it carried through", grant.RefreshToken)
	}
}
