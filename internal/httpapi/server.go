// Package httpapi exposes the submission endpoint, static frontend, and the
// review/failed queues, with per-IP rate limiting and structured logging.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
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

	// bridge state is a temporary Phase-1 scaffold: a single seeded dev user
	// injected into every request so the app runs before real auth lands in
	// Phase 2, which replaces devUser with requireAuth and removes this.
	bridgeMu   sync.Mutex
	bridgeUser int64
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
	// api wraps a handler with the current-user middleware; every /api route
	// operates only on the authenticated user's data. (Phase 1: devUser seeds
	// and injects one user; Phase 2 swaps in requireAuth.)
	api := func(h http.HandlerFunc) http.Handler { return s.devUser(h) }

	mux.Handle("POST /api/submit", s.rateLimit(api(s.handleSubmit)))
	mux.Handle("POST /api/submissions/{id}/confirm", s.rateLimit(api(s.handleConfirm)))
	mux.Handle("POST /api/submissions/{id}/discard", api(s.handleDiscard))
	mux.Handle("GET /api/submissions/{id}/image", api(s.handleImage))
	mux.Handle("GET /api/queue", api(s.handleQueue))
	mux.Handle("GET /api/preview", api(s.handlePreview))
	mux.Handle("GET /api/destination", api(s.handleDestination))
	mux.Handle("GET /api/history", api(s.handleHistory))
	mux.Handle("GET /", http.FileServerFS(static))
	return s.logging(mux)
}
