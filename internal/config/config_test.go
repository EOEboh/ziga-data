package config

import (
	"os"
	"path/filepath"
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
