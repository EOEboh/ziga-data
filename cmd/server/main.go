package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ziga "github.com/EOEboh/ziga-data"
	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/extract"
	"github.com/EOEboh/ziga-data/internal/httpapi"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/secretbox"
	"github.com/EOEboh/ziga-data/internal/sheets"
	"github.com/EOEboh/ziga-data/internal/store"
)

// dryRunWriter stands in for Google Sheets when SHEET_ID or credentials are
// not configured: an in-memory sheet, so the full submit → confirm → preview
// flow works locally. Rows are lost on restart. A cell containing the literal
// "[fail]" makes Append error, to exercise the UI's failed-write path. It
// mirrors the real writer's header semantics: with header set, the first
// append into the empty sheet writes the header row first, and LastRows
// skips it.
type dryRunWriter struct {
	log    *slog.Logger
	header []string
	mu     sync.Mutex
	rows   [][]string
}

func (d *dryRunWriter) Append(_ context.Context, row []string) error {
	for _, cell := range row {
		if strings.Contains(cell, "[fail]") {
			return errors.New("dry-run: simulated sheet failure ([fail] marker in a cell)")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.header != nil && len(d.rows) == 0 {
		d.rows = append(d.rows, d.header)
	}
	d.rows = append(d.rows, row)
	d.log.Info("dry-run: row stored in memory, sheets not configured", "row", row)
	return nil
}

func (d *dryRunWriter) LastRows(_ context.Context, n int) ([][]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := d.rows
	if d.header != nil && len(rows) > 0 {
		rows = rows[1:]
	}
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	out := make([][]string, len(rows))
	copy(out, rows)
	return out, nil
}

// DryRun marks the destination as not connected to a real sheet; the
// destination picker surfaces this.
func (d *dryRunWriter) DryRun() bool { return true }

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.OpenAIAPIKey == "" {
		log.Error("OPENAI_API_KEY is required")
		os.Exit(1)
	}
	// SESSION_SECRET keys CSRF token signatures. Without it, generate an
	// ephemeral secret so the app still runs locally — but sessions and CSRF
	// tokens won't survive a restart, so it must be set in production.
	if cfg.SessionSecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			log.Error("generate session secret", "err", err)
			os.Exit(1)
		}
		cfg.SessionSecret = base64.StdEncoding.EncodeToString(buf)
		log.Warn("SESSION_SECRET not set — generated an ephemeral one; sessions and CSRF tokens will not survive a restart")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// The SQLite file is the only local state — logged so backup scripts can
	// pick the path up from the boot output.
	dbPath := cfg.DBPath
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	log.Info("sqlite store open", "path", dbPath)

	// Retention: raw originals (full input text, image blobs) are purged
	// RETENTION_DAYS after a submission settles; extraction results stay.
	purge := func() {
		cutoff := time.Now().UTC().Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
		n, err := st.PurgeInputs(context.Background(), cutoff)
		if err != nil {
			log.Error("retention purge", "err", err)
			return
		}
		if n > 0 {
			log.Info("retention purge", "purged", n, "retention_days", cfg.RetentionDays)
		}
	}
	purge()
	go func() {
		for range time.Tick(24 * time.Hour) {
			purge()
		}
	}()

	extractor := llm.NewOpenAIExtractor(
		cfg.LLMModel,
		extract.SystemPrompt(cfg.Schema),
		extract.JSONSchema(cfg.Schema),
		func(text string, in llm.Input) string {
			return extract.UserText(text, in.SubmissionDate)
		},
	)

	// In header mode the writers maintain a header row of the schema's
	// column names; nil disables header handling entirely.
	var header []string
	if cfg.HeaderRow {
		header = cfg.Schema.Columns
	}

	var writer httpapi.RowWriter
	if cfg.SheetID != "" && cfg.GoogleCredsPath != "" {
		writer, err = sheets.NewWriter(context.Background(), cfg.GoogleCredsPath, cfg.SheetID, cfg.SheetTab, header)
		if err != nil {
			log.Error("sheets", "err", err)
			os.Exit(1)
		}
	} else {
		log.Warn("SHEET_ID / GOOGLE_APPLICATION_CREDENTIALS not set — running in dry-run mode, rows will not be written")
		writer = &dryRunWriter{log: log, header: header}
	}

	// Mailer for verification / password-reset links. Without SMTP configured,
	// the dev mailer logs links instead of sending them.
	var mailer mail.Mailer
	if cfg.SMTPHost != "" {
		mailer = mail.NewSMTPMailer(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom)
	} else {
		log.Warn("SMTP_HOST not set — verification and reset emails will be logged, not sent")
		mailer = mail.NewLogMailer(log)
	}

	// Google OAuth (identity + drive.file) and token encryption. When OAuth is
	// unconfigured (dev) the box stays nil and the OAuth routes report 404.
	oauthCfg := oauth.NewConfig(cfg.GoogleOAuthClientID, cfg.GoogleOAuthClientSecret, cfg.OAuthRedirectURL)
	var box *secretbox.Box
	if cfg.TokenEncryptionKey != "" {
		box, err = secretbox.New(cfg.TokenEncryptionKey)
		if err != nil {
			log.Error("token encryption key", "err", err)
			os.Exit(1)
		}
	}
	if oauthCfg.Configured() {
		log.Info("google oauth enabled", "scopes", oauthCfg.Scopes())
	}

	static, err := fs.Sub(ziga.WebFS, "web/dist")
	if err != nil {
		log.Error("embed", "err", err)
		os.Exit(1)
	}

	srv := httpapi.New(cfg, log, extractor, st, writer, mailer, oauthCfg, box)
	addr := ":" + cfg.Port
	log.Info("listening", "addr", addr, "model", cfg.LLMModel, "schema", cfg.Schema.Name)
	if err := http.ListenAndServe(addr, srv.Handler(static)); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
