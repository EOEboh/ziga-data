package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimalSchema = `{
	"name": "t",
	"required_fields": ["contact"],
	"fields": [{"name": "contact", "type": "string", "nullable": true, "description": "d"}],
	"columns": ["contact"]
}`

// setup creates a temp working dir with a schema file and points SCHEMA_PATH
// at it, so Load() can run without the repo's config/.
func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, []byte(minimalSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHEMA_PATH", schemaPath)
	t.Setenv("OPENAI_API_KEY", "") // isolate from the outer environment
	// Config tests are not production: default to dev mode so Load() doesn't trip
	// the OAuth-required boot guard. The guard itself is covered by
	// TestOAuthRequiredUnlessDevMode, which sets ZIGA_DEV_MODE explicitly.
	t.Setenv("ZIGA_DEV_MODE", "true")
	t.Chdir(dir)
	return dir
}

func TestLoadReadsDotEnvFile(t *testing.T) {
	dir := setup(t)
	err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=sk-from-dotenv\nSHEET_TAB=FromEnvFile\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("OPENAI_API_KEY") // Setenv("") above set it to empty; clear fully

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIAPIKey != "sk-from-dotenv" {
		t.Fatalf("OpenAIAPIKey = %q, want value from .env file", cfg.OpenAIAPIKey)
	}
	if cfg.SheetTab != "FromEnvFile" {
		t.Fatalf("SheetTab = %q, want value from .env file", cfg.SheetTab)
	}
}

func TestShellEnvWinsOverDotEnv(t *testing.T) {
	dir := setup(t)
	err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=sk-from-dotenv\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-from-shell")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIAPIKey != "sk-from-shell" {
		t.Fatalf("OpenAIAPIKey = %q, exported env must win over .env", cfg.OpenAIAPIKey)
	}
}

func TestLoadWithoutDotEnvStillWorks(t *testing.T) {
	setup(t)
	t.Setenv("OPENAI_API_KEY", "sk-plain")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load must not fail when no .env exists: %v", err)
	}
	if cfg.OpenAIAPIKey != "sk-plain" {
		t.Fatalf("OpenAIAPIKey = %q", cfg.OpenAIAPIKey)
	}
}

func TestHeaderRow(t *testing.T) {
	setup(t)

	for _, v := range []string{"", "1", "true", "TRUE"} {
		t.Setenv("HEADER_ROW", v)
		cfg, err := Load()
		if err != nil || !cfg.HeaderRow {
			t.Fatalf("HEADER_ROW=%q: HeaderRow=%v err=%v, want true", v, cfg != nil && cfg.HeaderRow, err)
		}
	}
	for _, v := range []string{"0", "none", "false", "None"} {
		t.Setenv("HEADER_ROW", v)
		cfg, err := Load()
		if err != nil || cfg.HeaderRow {
			t.Fatalf("HEADER_ROW=%q: HeaderRow=%v err=%v, want false", v, cfg != nil && cfg.HeaderRow, err)
		}
	}
	t.Setenv("HEADER_ROW", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("invalid HEADER_ROW must be rejected")
	}
}

func TestOAuthRequiredUnlessDevMode(t *testing.T) {
	// A full OAuth set, so the guard is satisfied when dev mode is off.
	setOAuth := func(t *testing.T) {
		t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "cid")
		t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "secret")
		t.Setenv("OAUTH_REDIRECT_URL", "http://localhost:8080/api/auth/google/callback")
		t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=") // 32 bytes
	}

	t.Run("prod without oauth is rejected and names the vars", func(t *testing.T) {
		setup(t)
		t.Setenv("OPENAI_API_KEY", "sk")
		t.Setenv("ZIGA_DEV_MODE", "false")
		_, err := Load()
		if err == nil {
			t.Fatal("Load must fail when OAuth is unconfigured and dev mode is off")
		}
		if !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_ID") {
			t.Fatalf("error must name the missing var, got: %v", err)
		}
	})

	t.Run("prod with full oauth boots", func(t *testing.T) {
		setup(t)
		t.Setenv("OPENAI_API_KEY", "sk")
		t.Setenv("ZIGA_DEV_MODE", "false")
		setOAuth(t)
		if _, err := Load(); err != nil {
			t.Fatalf("Load must succeed with full OAuth in prod mode: %v", err)
		}
	})

	t.Run("dev mode allows missing oauth", func(t *testing.T) {
		setup(t) // sets ZIGA_DEV_MODE=true
		t.Setenv("OPENAI_API_KEY", "sk")
		if _, err := Load(); err != nil {
			t.Fatalf("Load must succeed in dev mode without OAuth: %v", err)
		}
	})

	t.Run("invalid dev mode value is rejected", func(t *testing.T) {
		setup(t)
		t.Setenv("OPENAI_API_KEY", "sk")
		t.Setenv("ZIGA_DEV_MODE", "banana")
		if _, err := Load(); err == nil {
			t.Fatal("invalid ZIGA_DEV_MODE must be rejected")
		}
	})
}

func TestRetentionDays(t *testing.T) {
	setup(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RetentionDays != 14 {
		t.Fatalf("RetentionDays default = %d, want 14", cfg.RetentionDays)
	}

	t.Setenv("RETENTION_DAYS", "30")
	cfg, err = Load()
	if err != nil || cfg.RetentionDays != 30 {
		t.Fatalf("RetentionDays = %d err=%v, want 30", cfg.RetentionDays, err)
	}

	for _, bad := range []string{"0", "-1", "abc"} {
		t.Setenv("RETENTION_DAYS", bad)
		if _, err := Load(); err == nil {
			t.Fatalf("RETENTION_DAYS=%q must be rejected", bad)
		}
	}
}
