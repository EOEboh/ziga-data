package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	ziga "github.com/EOEboh/ziga"
	"github.com/EOEboh/ziga/internal/config"
	"github.com/EOEboh/ziga/internal/extract"
	"github.com/EOEboh/ziga/internal/httpapi"
	"github.com/EOEboh/ziga/internal/llm"
	"github.com/EOEboh/ziga/internal/sheets"
	"github.com/EOEboh/ziga/internal/store"
)

// dryRunWriter stands in for Google Sheets when SHEET_ID or credentials are
// not configured: an in-memory sheet, so the full submit → confirm → preview
// flow works locally. Rows are lost on restart. A cell containing the literal
// "[fail]" makes Append error, to exercise the UI's failed-write path.
type dryRunWriter struct {
	log  *slog.Logger
	mu   sync.Mutex
	rows [][]string
}

func (d *dryRunWriter) Append(_ context.Context, row []string) error {
	for _, cell := range row {
		if strings.Contains(cell, "[fail]") {
			return errors.New("dry-run: simulated sheet failure ([fail] marker in a cell)")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rows = append(d.rows, row)
	d.log.Info("dry-run: row stored in memory, sheets not configured", "row", row)
	return nil
}

func (d *dryRunWriter) LastRows(_ context.Context, n int) ([][]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := d.rows
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

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

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

	var writer httpapi.RowWriter
	if cfg.SheetID != "" && cfg.GoogleCredsPath != "" {
		writer, err = sheets.NewWriter(context.Background(), cfg.GoogleCredsPath, cfg.SheetID, cfg.SheetTab)
		if err != nil {
			log.Error("sheets", "err", err)
			os.Exit(1)
		}
	} else {
		log.Warn("SHEET_ID / GOOGLE_APPLICATION_CREDENTIALS not set — running in dry-run mode, rows will not be written")
		writer = &dryRunWriter{log: log}
	}

	static, err := fs.Sub(ziga.WebFS, "web")
	if err != nil {
		log.Error("embed", "err", err)
		os.Exit(1)
	}

	srv := httpapi.New(cfg, log, extractor, st, writer)
	addr := ":" + cfg.Port
	log.Info("listening", "addr", addr, "model", cfg.LLMModel, "schema", cfg.Schema.Name)
	if err := http.ListenAndServe(addr, srv.Handler(static)); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
