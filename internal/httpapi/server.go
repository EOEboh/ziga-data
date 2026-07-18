// Package httpapi exposes the submission endpoint, static frontend, and the
// review/failed queues, with per-IP rate limiting and structured logging.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/EOEboh/ziga/internal/config"
	"github.com/EOEboh/ziga/internal/llm"
	"github.com/EOEboh/ziga/internal/store"
)

// RowWriter appends rows to the destination sheet and reads the tail back
// for the preview strip. Implemented by the sheets package; stubbed in tests.
type RowWriter interface {
	Append(ctx context.Context, row []string) error
	LastRows(ctx context.Context, n int) ([][]string, error)
}

type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	extractor llm.Extractor
	store     *store.Store
	writer    RowWriter
	// limiter is the single per-IP budget shared by every rate-limited
	// endpoint: submit (LLM cost) and confirm (Google Sheets quota).
	limiter *ipLimiter
	// confirmMu serializes confirms; concurrent confirms of the same
	// submission would otherwise append duplicate sheet rows.
	confirmMu sync.Mutex
}

func New(cfg *config.Config, log *slog.Logger, ex llm.Extractor, st *store.Store, w RowWriter) *Server {
	return &Server{cfg: cfg, log: log, extractor: ex, store: st, writer: w, limiter: newIPLimiter(cfg.RatePerMin)}
}

// Handler builds the full route tree. static is the embedded web/ directory.
func (s *Server) Handler(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("POST /api/submit", s.rateLimit(http.HandlerFunc(s.handleSubmit)))
	mux.Handle("POST /api/submissions/{id}/confirm", s.rateLimit(http.HandlerFunc(s.handleConfirm)))
	mux.HandleFunc("POST /api/submissions/{id}/discard", s.handleDiscard)
	mux.HandleFunc("GET /api/submissions/{id}/image", s.handleImage)
	mux.HandleFunc("GET /api/queue", s.handleQueue)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	mux.HandleFunc("GET /api/destination", s.handleDestination)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.Handle("GET /", http.FileServerFS(static))
	return s.logging(mux)
}
