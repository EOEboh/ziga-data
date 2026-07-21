// Package httpapi exposes the submission endpoint, static frontend, and the
// review/failed queues, with per-IP rate limiting and structured logging.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/mail"
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
	mailer    mail.Mailer
	// limiter is the single per-IP budget shared by every rate-limited
	// endpoint: submit (LLM cost) and confirm (Google Sheets quota).
	limiter *ipLimiter
	// loginLimiter and resetLimiter are stricter per-IP budgets protecting the
	// login and password-reset endpoints from brute force, separate from the
	// API limiter above.
	loginLimiter *ipLimiter
	resetLimiter *ipLimiter
	// confirmMu serializes confirms; concurrent confirms of the same
	// submission would otherwise append duplicate sheet rows.
	confirmMu sync.Mutex

	// sessionSecret keys CSRF token HMACs; secureCookies marks cookies Secure
	// when the app is served over https; baseURL builds email links.
	sessionSecret []byte
	secureCookies bool
	baseURL       string
}

func New(cfg *config.Config, log *slog.Logger, ex llm.Extractor, st *store.Store, w RowWriter, m mail.Mailer) *Server {
	return &Server{
		cfg: cfg, log: log, extractor: ex, store: st, writer: w, mailer: m,
		limiter:       newIPLimiter(cfg.RatePerMin),
		loginLimiter:  newIPLimiterBurst(20, 5),
		resetLimiter:  newIPLimiterBurst(6, 3),
		sessionSecret: []byte(cfg.SessionSecret),
		secureCookies: strings.HasPrefix(cfg.AppBaseURL, "https://"),
		baseURL:       strings.TrimRight(cfg.AppBaseURL, "/"),
	}
}

// Handler builds the full route tree. static is the embedded web/ directory.
func (s *Server) Handler(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// public routes carry CSRF (so unsafe methods are protected and a token
	// cookie is issued) but no session requirement. protected routes add
	// requireAuth, which injects the current user; every user-scoped handler
	// then operates only on that user's data.
	public := func(h http.HandlerFunc) http.Handler { return s.csrf(h) }
	protected := func(h http.HandlerFunc) http.Handler { return s.csrf(s.requireAuth(h)) }

	// Auth (public). Login and password-reset are additionally rate-limited.
	mux.Handle("POST /api/auth/signup", public(s.handleSignup))
	mux.Handle("GET /api/auth/verify", public(s.handleVerifyEmail))
	mux.Handle("POST /api/auth/login", s.rateLimitFor(s.loginLimiter, public(s.handleLogin)))
	mux.Handle("POST /api/auth/logout", public(s.handleLogout))
	mux.Handle("POST /api/auth/password/forgot", s.rateLimitFor(s.resetLimiter, public(s.handleForgotPassword)))
	mux.Handle("POST /api/auth/password/reset", public(s.handleResetPassword))
	mux.Handle("GET /api/me", public(s.handleMe))

	// Submission app (protected + user-scoped).
	mux.Handle("POST /api/submit", s.rateLimit(protected(s.handleSubmit)))
	mux.Handle("POST /api/submissions/{id}/confirm", s.rateLimit(protected(s.handleConfirm)))
	mux.Handle("POST /api/submissions/{id}/discard", protected(s.handleDiscard))
	mux.Handle("GET /api/submissions/{id}/image", protected(s.handleImage))
	mux.Handle("GET /api/queue", protected(s.handleQueue))
	mux.Handle("GET /api/preview", protected(s.handlePreview))
	mux.Handle("GET /api/destination", protected(s.handleDestination))
	mux.Handle("GET /api/history", protected(s.handleHistory))

	mux.Handle("GET /", http.FileServerFS(static))
	return s.logging(mux)
}
