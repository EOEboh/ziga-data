package main

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	sheetdrop "github.com/EOEboh/sheetdrop"
	"github.com/EOEboh/sheetdrop/internal/config"
	"github.com/EOEboh/sheetdrop/internal/extract"
	"github.com/EOEboh/sheetdrop/internal/httpapi"
	"github.com/EOEboh/sheetdrop/internal/llm"
	"github.com/EOEboh/sheetdrop/internal/sheets"
	"github.com/EOEboh/sheetdrop/internal/store"
)

// dryRunWriter stands in for Google Sheets when SHEET_ID or credentials are
// not configured — useful for local testing of the extraction path.
type dryRunWriter struct{ log *slog.Logger }

func (d dryRunWriter) Append(_ context.Context, row []string) error {
	d.log.Warn("dry-run: sheets not configured, row not written", "row", row)
	return nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.AnthropicAPIKey == "" {
		log.Error("ANTHROPIC_API_KEY is required")
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	extractor := llm.NewAnthropicExtractor(
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
		writer = dryRunWriter{log: log}
	}

	static, err := fs.Sub(sheetdrop.WebFS, "web")
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
