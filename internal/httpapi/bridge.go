package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/EOEboh/ziga-data/internal/store"
)

// TEMPORARY (Phase 1 scaffold). Until real authentication lands in Phase 2,
// every /api request is attributed to a single seeded "dev" user so the
// existing submit → review → confirm flow keeps working while the store is
// now user-scoped. Phase 2 deletes this file and replaces devUser with
// requireAuth (a real session lookup).

const bridgeUserEmail = "dev@local"

// devUser injects the seeded dev user's id into the request context.
func (s *Server) devUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := s.ensureBridgeUser(r.Context())
		if err != nil {
			s.log.Error("bridge user", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), uid)))
	})
}

// ensureBridgeUser returns the seeded dev user's id, creating it (verified)
// once. Cached on the Server after the first call.
func (s *Server) ensureBridgeUser(ctx context.Context) (int64, error) {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	if s.bridgeUser != 0 {
		return s.bridgeUser, nil
	}
	u, err := s.store.GetUserByEmail(ctx, bridgeUserEmail)
	if errors.Is(err, store.ErrNotFound) {
		u, err = s.store.CreateUser(ctx, bridgeUserEmail, "")
		if err != nil {
			return 0, err
		}
		if err := s.store.MarkEmailVerified(ctx, u.ID); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	s.bridgeUser = u.ID
	return u.ID, nil
}
