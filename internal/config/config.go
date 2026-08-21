// Package config loads server configuration from environment variables and
// the extraction schema definition from a JSON file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Field is one extractable field in the schema config.
type Field struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Nullable    bool   `json:"nullable"`
	Description string `json:"description"`
}

// Schema is the extraction schema plus the sheet column mapping. In v1 there
// is exactly one schema, but nothing outside this package assumes that.
type Schema struct {
	Name           string   `json:"name"`
	RequiredFields []string `json:"required_fields"`
	Fields         []Field  `json:"fields"`
	Columns        []string `json:"columns"`
}

// Config is the full server configuration.
type Config struct {
	OpenAIAPIKey    string
	LLMModel        string
	GoogleCredsPath string
	SheetID         string
	SheetTab        string
	Port            string
	DBPath          string
	RatePerMin      int
	RetentionDays   int
	// HeaderRow: row 1 of the sheet tab is a header (written automatically
	// on first confirm if the tab is empty). False = the tab has no header.
	HeaderRow  bool
	SchemaPath string
	Schema     Schema

	// DevMode (ZIGA_DEV_MODE) relaxes the production boot guard. When false (the
	// default) the app refuses to start unless Google OAuth is fully configured,
	// so a misnamed OAuth var can never silently disable the production write
	// path. When true the in-memory dry-run fallback is allowed.
	DevMode bool

	// Auth / multi-tenant configuration.
	// SessionSecret keys the HMAC on CSRF tokens. If empty at boot an ephemeral
	// one is generated (sessions/CSRF then don't survive a restart) — set it in
	// production.
	SessionSecret string
	// AppBaseURL is the public origin, used to build email links and to decide
	// whether cookies get the Secure attribute (https).
	AppBaseURL string
	// SMTP_* configure the outbound mailer. When SMTPHost is empty the app uses
	// a dev mailer that logs verification/reset links instead of sending them.
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// Google OAuth (identity + drive.file). When ClientID/Secret are empty the
	// app runs without Google sign-in or per-user Sheets (dev / dry-run).
	GoogleOAuthClientID     string
	GoogleOAuthClientSecret string
	OAuthRedirectURL        string
	// GooglePickerAPIKey is a browser API key served to the frontend for the
	// Google Picker (attach-existing-sheet flow).
	GooglePickerAPIKey string
	// GoogleProjectNumber is the numeric Google Cloud project number (the prefix
	// of the OAuth client id). Served to the frontend and passed to the Picker's
	// setAppId so a picked file's drive.file grant is attributed to this app —
	// without it the server's stored token cannot read the picked spreadsheet.
	GoogleProjectNumber string
	// TokenEncryptionKey (base64, 32 bytes) encrypts OAuth tokens at rest.
	// Required whenever any OAuth provider is configured.
	TokenEncryptionKey string

	// Notion OAuth (public integration). When these are empty, Notion is not
	// offered as a destination at all; partially set is a boot error, so a
	// typo can never advertise a flow that cannot complete.
	NotionOAuthClientID     string
	NotionOAuthClientSecret string
	NotionOAuthRedirectURL  string
	// NotionVersion is the Notion-Version header sent on every Notion API
	// request. Notion versions by date and pins behavior to the version
	// string, so it lives here as one value rather than at the call sites.
	NotionVersion string

	// Email ingestion. When these are empty the feature is off and
	// POST /api/ingest/email is not registered at all, so an unconfigured
	// server 404s rather than exposing an unauthenticated route.
	//
	// InboundEmailDomain is the domain users' capture addresses live on
	// (e.g. in.zigadata.com). It must be a domain used for nothing else:
	// enabling mail routing on it changes its MX records.
	InboundEmailDomain string
	// IngestSharedSecret keys the HMAC on the ingestion webhook. Shared with
	// the mail worker; never sent anywhere else.
	IngestSharedSecret string
	// CloudflareAPIToken / CloudflareZoneID / IngestWorkerName let the app
	// provision a routing rule per address. A per-address rule is required
	// because catch-all is not available on a subdomain, only at a zone apex.
	CloudflareAPIToken string
	CloudflareZoneID   string
	IngestWorkerName   string
	// IngestDailyCap and IngestBurst bound how much mail one user can push
	// through extraction. This is the primary defence against an unbounded
	// model bill: a user who forwards their whole inbox by accident hits the
	// cap rather than the invoice.
	IngestDailyCap int
	IngestBurst    int
	// IngestMaxAddresses caps how many capture addresses exist at once, kept
	// below the mail provider's per-domain routing-rule limit so provisioning
	// fails with our clear error rather than their opaque one.
	IngestMaxAddresses int
}

// minIngestSecretLen is the shortest shared secret Load will accept. The
// ingestion endpoint is unauthenticated apart from this HMAC and every call
// costs money, so a guessable key is a boot failure rather than a warning.
const minIngestSecretLen = 32

// EmailIngestConfigured reports whether email ingestion is fully configured
// and can be offered. Load guarantees this is all-or-nothing.
func (c *Config) EmailIngestConfigured() bool {
	return c.InboundEmailDomain != "" && c.IngestSharedSecret != "" &&
		c.CloudflareAPIToken != "" && c.CloudflareZoneID != "" && c.IngestWorkerName != ""
}

// OAuthConfigured reports whether Google OAuth credentials are present.
func (c *Config) OAuthConfigured() bool {
	return c.GoogleOAuthClientID != "" && c.GoogleOAuthClientSecret != ""
}

// NotionConfigured reports whether Notion is fully configured and can be
// offered as a destination. Load guarantees this is all-or-nothing.
func (c *Config) NotionConfigured() bool {
	return c.NotionOAuthClientID != "" && c.NotionOAuthClientSecret != "" && c.NotionOAuthRedirectURL != ""
}

// DefaultNotionVersion is the Notion API version this build targets.
//
// Version 2025-09-03 introduced data sources: a database is a parent of one or
// more data sources, the property schema lives on the data source, and pages
// are created with a data_source_id parent. The older 2022-06-28 model is
// simpler but Notion documents it as failing outright once a database gains a
// second data source — which would break lead writes for a user who merely
// restructured their database. This build targets the data-source model.
const DefaultNotionVersion = "2026-03-11"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads env vars and the schema file. It fails fast on anything that
// would only surface as a runtime error later.
//
// A .env file in the working directory is loaded first, but variables
// already exported in the environment always take precedence over it.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	cfg := &Config{
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		LLMModel:        envOr("LLM_MODEL", "gpt-5.4-nano"),
		GoogleCredsPath: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		SheetID:         os.Getenv("SHEET_ID"),
		SheetTab:        envOr("SHEET_TAB", "Leads"),
		Port:            envOr("PORT", "8080"),
		DBPath:          envOr("DB_PATH", "./ziga.db"),
		SchemaPath:      envOr("SCHEMA_PATH", "config/schema.json"),
		RatePerMin:      10,
		RetentionDays:   14,
		SessionSecret:   os.Getenv("SESSION_SECRET"),
		AppBaseURL:      envOr("APP_BASE_URL", "http://localhost:8080"),
		SMTPHost:        os.Getenv("SMTP_HOST"),
		SMTPPort:        envOr("SMTP_PORT", "587"),
		SMTPUsername:    os.Getenv("SMTP_USERNAME"),
		SMTPPassword:    os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:        envOr("SMTP_FROM", "ziga@localhost"),

		GoogleOAuthClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleOAuthClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		OAuthRedirectURL:        envOr("OAUTH_REDIRECT_URL", envOr("APP_BASE_URL", "http://localhost:8080")+"/api/auth/google/callback"),
		GooglePickerAPIKey:      os.Getenv("GOOGLE_PICKER_API_KEY"),
		GoogleProjectNumber:     os.Getenv("GOOGLE_PROJECT_NUMBER"),
		TokenEncryptionKey:      os.Getenv("TOKEN_ENCRYPTION_KEY"),

		NotionOAuthClientID:     os.Getenv("NOTION_OAUTH_CLIENT_ID"),
		NotionOAuthClientSecret: os.Getenv("NOTION_OAUTH_CLIENT_SECRET"),
		NotionOAuthRedirectURL:  os.Getenv("NOTION_OAUTH_REDIRECT_URL"),
		NotionVersion:           envOr("NOTION_VERSION", DefaultNotionVersion),

		InboundEmailDomain: strings.ToLower(strings.TrimSpace(os.Getenv("INBOUND_EMAIL_DOMAIN"))),
		IngestSharedSecret: os.Getenv("INGEST_SHARED_SECRET"),
		CloudflareAPIToken: os.Getenv("CLOUDFLARE_API_TOKEN"),
		CloudflareZoneID:   os.Getenv("CLOUDFLARE_ZONE_ID"),
		IngestWorkerName:   envOr("INGEST_WORKER_NAME", "ziga-email-ingest"),
		IngestDailyCap:     50,
		IngestBurst:        10,
		IngestMaxAddresses: 180,
	}
	switch v := strings.ToLower(os.Getenv("ZIGA_DEV_MODE")); v {
	case "", "0", "false":
		cfg.DevMode = false
	case "1", "true":
		cfg.DevMode = true
	default:
		return nil, fmt.Errorf("invalid ZIGA_DEV_MODE %q (want 1/true or 0/false)", v)
	}
	// Production boot guard: unless ZIGA_DEV_MODE=true, Google OAuth must be fully
	// configured. This is what stops a typo'd OAuth var name from booting a
	// "healthy" process that silently falls through to the dry-run writer.
	if !cfg.DevMode {
		var missing []string
		if cfg.GoogleOAuthClientID == "" {
			missing = append(missing, "GOOGLE_OAUTH_CLIENT_ID")
		}
		if cfg.GoogleOAuthClientSecret == "" {
			missing = append(missing, "GOOGLE_OAUTH_CLIENT_SECRET")
		}
		if cfg.OAuthRedirectURL == "" {
			missing = append(missing, "OAUTH_REDIRECT_URL")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("Google OAuth is required unless ZIGA_DEV_MODE=true; missing: %s", strings.Join(missing, ", "))
		}
	}
	// Notion is all-or-nothing. Offering "Connect Notion" in the UI and then
	// failing at the callback because one var was misnamed is exactly the
	// failure the Google guard above exists to prevent, so partial Notion
	// configuration refuses to boot. Setting none of them is fine: Notion is
	// then simply not offered.
	notionVars := map[string]string{
		"NOTION_OAUTH_CLIENT_ID":     cfg.NotionOAuthClientID,
		"NOTION_OAUTH_CLIENT_SECRET": cfg.NotionOAuthClientSecret,
		"NOTION_OAUTH_REDIRECT_URL":  cfg.NotionOAuthRedirectURL,
	}
	var setNotion, missingNotion []string
	for _, name := range []string{"NOTION_OAUTH_CLIENT_ID", "NOTION_OAUTH_CLIENT_SECRET", "NOTION_OAUTH_REDIRECT_URL"} {
		if notionVars[name] == "" {
			missingNotion = append(missingNotion, name)
		} else {
			setNotion = append(setNotion, name)
		}
	}
	if len(setNotion) > 0 && len(missingNotion) > 0 {
		return nil, fmt.Errorf("Notion OAuth is partially configured (%s set); missing: %s",
			strings.Join(setNotion, ", "), strings.Join(missingNotion, ", "))
	}
	if cfg.NotionVersion == "" {
		return nil, fmt.Errorf("NOTION_VERSION must not be empty; Notion requires a version on every request")
	}

	// When any OAuth provider is configured, the token-encryption key is
	// mandatory — we must never store OAuth tokens in plaintext.
	if (cfg.OAuthConfigured() || cfg.NotionConfigured()) && cfg.TokenEncryptionKey == "" {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is required when OAuth is configured")
	}

	// Email ingestion is all-or-nothing, for the same reason as Notion above
	// but with sharper failure modes: a domain with no shared secret would
	// register an ingestion endpoint that anyone on the internet could post
	// leads to, and a secret with no Cloudflare credentials would hand users
	// an address that no mail can ever reach. Setting none of them is fine —
	// ingestion is then simply not offered.
	ingestVars := []struct {
		name, value string
	}{
		{"INBOUND_EMAIL_DOMAIN", cfg.InboundEmailDomain},
		{"INGEST_SHARED_SECRET", cfg.IngestSharedSecret},
		{"CLOUDFLARE_API_TOKEN", cfg.CloudflareAPIToken},
		{"CLOUDFLARE_ZONE_ID", cfg.CloudflareZoneID},
		{"INGEST_WORKER_NAME", cfg.IngestWorkerName},
	}
	var setIngest, missingIngest []string
	for _, v := range ingestVars {
		if v.value == "" {
			missingIngest = append(missingIngest, v.name)
		} else {
			setIngest = append(setIngest, v.name)
		}
	}
	// INGEST_WORKER_NAME has a default, so it is always set; it only counts as
	// evidence of intent when something else is too.
	if len(setIngest) > 1 && len(missingIngest) > 0 {
		return nil, fmt.Errorf("email ingestion is partially configured (%s set); missing: %s",
			strings.Join(setIngest, ", "), strings.Join(missingIngest, ", "))
	}
	// The shared secret is the only thing standing in front of an endpoint
	// that costs money to call, so a weak one is refused rather than warned
	// about.
	if cfg.IngestSharedSecret != "" && len(cfg.IngestSharedSecret) < minIngestSecretLen {
		return nil, fmt.Errorf("INGEST_SHARED_SECRET must be at least %d characters (got %d); generate one with: head -c 32 /dev/urandom | base64",
			minIngestSecretLen, len(cfg.IngestSharedSecret))
	}
	for _, v := range []struct {
		env    string
		target *int
	}{
		{"INGEST_DAILY_CAP", &cfg.IngestDailyCap},
		{"INGEST_BURST", &cfg.IngestBurst},
		{"INGEST_MAX_ADDRESSES", &cfg.IngestMaxAddresses},
	} {
		if raw := os.Getenv(v.env); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid %s %q", v.env, raw)
			}
			*v.target = n
		}
	}
	if v := os.Getenv("RATE_LIMIT_PER_MIN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid RATE_LIMIT_PER_MIN %q", v)
		}
		cfg.RatePerMin = n
	}
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid RETENTION_DAYS %q", v)
		}
		cfg.RetentionDays = n
	}
	switch v := strings.ToLower(os.Getenv("HEADER_ROW")); v {
	case "", "1", "true":
		cfg.HeaderRow = true
	case "0", "none", "false":
		cfg.HeaderRow = false
	default:
		return nil, fmt.Errorf("invalid HEADER_ROW %q (want 1/true or 0/none/false)", v)
	}

	raw, err := os.ReadFile(cfg.SchemaPath)
	if err != nil {
		return nil, fmt.Errorf("read schema config: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg.Schema); err != nil {
		return nil, fmt.Errorf("parse schema config %s: %w", cfg.SchemaPath, err)
	}
	if len(cfg.Schema.Fields) == 0 || len(cfg.Schema.Columns) == 0 {
		return nil, fmt.Errorf("schema config %s must define fields and columns", cfg.SchemaPath)
	}
	return cfg, nil
}
