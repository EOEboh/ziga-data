// Package notionauth wraps Notion's OAuth 2.0 public-integration flow.
//
// It is a sibling of internal/oauth (Google) rather than a reuse of it,
// because Notion's flow differs in three ways that matter:
//
//   - The token endpoint requires HTTP Basic authentication with the client
//     id and secret, not credentials in the form body.
//   - The token response carries workspace identity (bot id, workspace id and
//     name) instead of an OpenID userinfo endpoint, so there is no second call
//     to resolve who connected.
//   - Access is granted per resource. During Notion's own consent screen the
//     user picks exactly which pages and databases the integration may touch;
//     the app never asks for, and never receives, whole-workspace access. This
//     mirrors the drive.file model used for Google.
//
// Access tokens are long-lived. A refresh token is only returned for
// integrations that opt into token rotation, so both the refresh token and the
// expiry are treated as optional.
package notionauth

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
)

// Notion's OAuth endpoints. Both live under the same api.notion.com host as
// the data API.
const (
	defaultAuthURL  = "https://api.notion.com/v1/oauth/authorize"
	defaultTokenURL = "https://api.notion.com/v1/oauth/token"
)

// Config is the configured Notion OAuth client.
type Config struct {
	oauth2 *oauth2.Config
}

// NewConfig builds the OAuth client. Configured is false when credentials are
// absent, letting the server report 404 on the Notion routes rather than
// offering a flow that cannot complete.
func NewConfig(clientID, clientSecret, redirectURL string) *Config {
	return &Config{
		oauth2: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			// Notion has no scope parameter: what the integration may reach is
			// chosen by the user, resource by resource, during consent.
			Endpoint: oauth2.Endpoint{
				AuthURL:   defaultAuthURL,
				TokenURL:  defaultTokenURL,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
	}
}

// Configured reports whether Notion OAuth credentials are present.
func (c *Config) Configured() bool {
	return c.oauth2.ClientID != "" && c.oauth2.ClientSecret != ""
}

// SetEndpoints overrides the authorization and token endpoints, to point at a
// fake Notion in tests.
func (c *Config) SetEndpoints(authURL, tokenURL string) {
	c.oauth2.Endpoint = oauth2.Endpoint{
		AuthURL: authURL, TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInHeader,
	}
}

// AuthCodeURL builds the consent URL. owner=user is required by Notion for the
// public-integration flow.
func (c *Config) AuthCodeURL(state string) string {
	return c.oauth2.AuthCodeURL(state, oauth2.SetAuthURLParam("owner", "user"))
}

// Grant is what a completed Notion authorization yields: the access token plus
// the identity of the workspace and bot it belongs to.
type Grant struct {
	AccessToken   string
	RefreshToken  string // empty unless the integration uses token rotation
	BotID         string
	WorkspaceID   string
	WorkspaceName string
	WorkspaceIcon string
}

// Exchange swaps an authorization code for a token and the workspace identity
// that came with it.
//
// Notion returns the workspace fields alongside the token rather than at a
// separate userinfo endpoint, so they are read off the raw token response.
func (c *Config) Exchange(ctx context.Context, code string) (*Grant, error) {
	tok, err := c.oauth2.Exchange(ctx, code,
		oauth2.SetAuthURLParam("redirect_uri", c.oauth2.RedirectURL))
	if err != nil {
		return nil, fmt.Errorf("notion token exchange: %w", err)
	}
	g := &Grant{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
	}
	// The workspace fields ride along as extras on the token response.
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"bot_id", &g.BotID},
		{"workspace_id", &g.WorkspaceID},
		{"workspace_name", &g.WorkspaceName},
		{"workspace_icon", &g.WorkspaceIcon},
	} {
		if v, ok := tok.Extra(f.key).(string); ok {
			*f.dst = v
		}
	}
	if g.AccessToken == "" {
		return nil, fmt.Errorf("notion token exchange: response carried no access token")
	}
	// The bot id is the stable per-install identity; without it a reconnect
	// could not be matched to the existing link.
	if g.BotID == "" {
		return nil, fmt.Errorf("notion token exchange: response carried no bot_id")
	}
	return g, nil
}

// RedirectURL exposes the configured callback, which must match the one
// registered on the Notion integration exactly.
func (c *Config) RedirectURL() string { return c.oauth2.RedirectURL }
