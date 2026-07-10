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
	SchemaPath      string
	Schema          Schema
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
		DBPath:          envOr("DB_PATH", "./sheetdrop.db"),
		SchemaPath:      envOr("SCHEMA_PATH", "config/schema.json"),
		RatePerMin:      10,
	}
	if v := os.Getenv("RATE_LIMIT_PER_MIN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid RATE_LIMIT_PER_MIN %q", v)
		}
		cfg.RatePerMin = n
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
