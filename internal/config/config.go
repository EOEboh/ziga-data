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
	// Required whenever Google OAuth is configured.
	TokenEncryptionKey string
}

// OAuthConfigured reports whether Google OAuth credentials are present.
func (c *Config) OAuthConfigured() bool {
	return c.GoogleOAuthClientID != "" && c.GoogleOAuthClientSecret != ""
}

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
	}
	// When Google OAuth is configured, the token-encryption key is mandatory —
	// we must never store OAuth tokens in plaintext.
	if cfg.OAuthConfigured() && cfg.TokenEncryptionKey == "" {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is required when Google OAuth is configured")
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
