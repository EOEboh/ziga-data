// Package httpapi exposes the submission endpoint, static frontend, and the
// review/failed queues, with per-IP rate limiting and structured logging.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/EOEboh/sheetdrop/internal/config"
	"github.com/EOEboh/sheetdrop/internal/llm"
	"github.com/EOEboh/sheetdrop/internal/store"
)

// RowWriter appends one row to the destination sheet. Implemented by the
// sheets package; stubbed in tests.
type RowWriter interface {
	Append(ctx context.Context, row []string) error
}

type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	extractor llm.Extractor
	store     *store.Store
	writer    RowWriter
}

func New(cfg *config.Config, log *slog.Logger, ex llm.Extractor, st *store.Store, w RowWriter) *Server {
	return &Server{cfg: cfg, log: log, extractor: ex, store: st, writer: w}
}

// Handler builds the full route tree. static is the embedded web/ directory.
func (s *Server) Handler(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("POST /api/submit", s.rateLimit(http.HandlerFunc(s.handleSubmit)))
	mux.HandleFunc("GET /api/review", s.handleQueue(store.StatusNeedsReview))
	mux.HandleFunc("GET /api/failed", s.handleQueue(store.StatusFailedWrite))
	mux.Handle("GET /", http.FileServerFS(static))
	return s.logging(mux)
}
