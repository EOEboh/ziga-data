package httpapi

import (
	"context"
	"net/http"
)

// ctxKey is an unexported type for request-context keys, so values set here
// can't collide with those from other packages.
type ctxKey int

const userIDKey ctxKey = iota

// withUser returns a copy of ctx carrying the authenticated user's id. Set by
// the auth middleware once per request.
func withUser(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// userID returns the authenticated user's id for the request, or 0 when none
// is set. Handlers behind the auth middleware always have a non-zero id;
// 0 means "unauthenticated" and should never reach a user-scoped store call.
func userID(r *http.Request) int64 {
	id, _ := r.Context().Value(userIDKey).(int64)
	return id
}
