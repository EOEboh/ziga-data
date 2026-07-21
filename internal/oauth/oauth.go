// Package oauth wraps the Google OAuth 2.0 authorization-code flow used for
// both identity (sign-in) and Google Sheets access. It requests only identity
// scopes plus drive.file — never the broad spreadsheets scope — so Google app
// verification stays light. Token refresh is transparent, with a hook so the
// caller can persist renewed tokens and react to a revoked grant.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scopes: OpenID identity + drive.file (per-file access to sheets the app
// creates or the user picks). Deliberately NOT the broad spreadsheets scope.
const (
	ScopeOpenID    = "openid"
	ScopeEmail     = "https://www.googleapis.com/auth/userinfo.email"
	ScopeProfile   = "https://www.googleapis.com/auth/userinfo.profile"
	ScopeDriveFile = "https://www.googleapis.com/auth/drive.file"
)

const defaultUserinfoURL = "https://openidconnect.googleapis.com/v1/userinfo"

// Config is the configured Google OAuth client.
type Config struct {
	oauth2      *oauth2.Config
	userinfoURL string
}

// NewConfig builds the OAuth client. Configured is false when credentials are
// absent (dev / dry-run), letting the server skip OAuth routes gracefully.
func NewConfig(clientID, clientSecret, redirectURL string) *Config {
	return &Config{
		oauth2: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       []string{ScopeOpenID, ScopeEmail, ScopeProfile, ScopeDriveFile},
			Endpoint:     google.Endpoint,
		},
		userinfoURL: defaultUserinfoURL,
	}
}

// Configured reports whether OAuth credentials are present.
func (c *Config) Configured() bool {
	return c.oauth2.ClientID != "" && c.oauth2.ClientSecret != ""
}

// SetEndpoints overrides the authorization/token/userinfo endpoints. Used to
// point at a fake Google server in tests (or a non-default environment).
func (c *Config) SetEndpoints(authURL, tokenURL, userinfoURL string) {
	c.oauth2.Endpoint = oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL}
	c.userinfoURL = userinfoURL
}

// ClientID exposes the public client id (served to the frontend for the Picker).
func (c *Config) ClientID() string { return c.oauth2.ClientID }

// Scopes exposes the requested scopes (for logging / diagnostics).
func (c *Config) Scopes() []string { return c.oauth2.Scopes }

// AuthCodeURL builds the consent URL. AccessTypeOffline + consent prompt ensure
// Google returns a refresh token (needed for long-lived Sheets writes).
func (c *Config) AuthCodeURL(state string) string {
	return c.oauth2.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"))
}

// Exchange swaps an authorization code for tokens.
func (c *Config) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	return c.oauth2.Exchange(ctx, code)
}

// UserInfo is the subset of the OpenID userinfo response we use.
type UserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// FetchUserInfo calls the OpenID userinfo endpoint with the given token.
func (c *Config) FetchUserInfo(ctx context.Context, tok *oauth2.Token) (*UserInfo, error) {
	client := c.oauth2.Client(ctx, tok)
	resp, err := client.Get(c.userinfoURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: unexpected status %d", resp.StatusCode)
	}
	var info UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.Sub == "" {
		return nil, fmt.Errorf("userinfo: missing subject")
	}
	return &info, nil
}

// TokenSource returns a refreshing token source seeded with tok. onRefresh is
// invoked with the new token whenever the access token is renewed, so the
// caller can persist it. A refresh failure (revoked access) surfaces as an
// error from Token(), which the caller maps to a "reconnect needed" state.
func (c *Config) TokenSource(ctx context.Context, tok *oauth2.Token, onRefresh func(*oauth2.Token)) oauth2.TokenSource {
	return &notifyingSource{
		base:      c.oauth2.TokenSource(ctx, tok),
		onRefresh: onRefresh,
		last:      tok,
	}
}

type notifyingSource struct {
	base      oauth2.TokenSource
	onRefresh func(*oauth2.Token)
	last      *oauth2.Token
}

func (n *notifyingSource) Token() (*oauth2.Token, error) {
	tok, err := n.base.Token()
	if err != nil {
		return nil, err
	}
	if n.last == nil || tok.AccessToken != n.last.AccessToken {
		if n.onRefresh != nil {
			n.onRefresh(tok)
		}
		n.last = tok
	}
	return tok, nil
}
