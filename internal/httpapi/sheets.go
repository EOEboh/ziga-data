package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/sheets"
	"github.com/EOEboh/ziga-data/internal/store"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// Typed resolver errors, mapped to user-facing responses by the handlers.
var (
	// errNoSheet: the user hasn't connected a destination yet (onboarding).
	errNoSheet = errors.New("no destination sheet connected")
	// errReconnect: the Google grant is missing/revoked; the user must reconnect.
	errReconnect = errors.New("google connection needs reconnect")
)

const spreadsheetTitle = "Ziga Leads"

// googleEnabled reports whether real per-user Google Sheets writing is active.
// When false the app is in dev / dry-run mode and uses the in-memory writer.
func (s *Server) googleEnabled() bool {
	return s.oauth != nil && s.oauth.Configured() && s.box != nil
}

// destinationsEnabled reports whether any real destination provider is
// configured. When false the app is in dev / dry-run mode: there are no
// per-user destinations and every write goes to the in-memory writer.
func (s *Server) destinationsEnabled() bool {
	return s.googleEnabled()
}

// header is the column-name row to maintain, or nil in no-header mode.
func (s *Server) header() []string {
	if s.cfg.HeaderRow {
		return s.cfg.Schema.Columns
	}
	return nil
}

// writerFor resolves the destination writer for a user by their destination
// type. In dev/dry-run it's the shared in-memory writer. Returns errNoSheet /
// errReconnect for the onboarding and reconnect states.
//
// This is the one place that knows which writer implementation serves which
// destination type; everything downstream works through destination.Writer.
func (s *Server) writerFor(ctx context.Context, uid int64) (destination.Writer, error) {
	if !s.destinationsEnabled() {
		return s.writer, nil
	}
	dest, err := s.store.GetDestination(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNoSheet
	}
	if err != nil {
		return nil, err
	}
	if dest.Broken() {
		return nil, errReconnect
	}
	switch destination.Type(dest.Type) {
	case destination.TypeGoogleSheet:
		return s.sheetWriter(ctx, uid, dest)
	default:
		return nil, fmt.Errorf("unknown destination type %q", dest.Type)
	}
}

// sheetWriter builds a per-user Google Sheets writer from the user's connected
// sheet and OAuth token.
func (s *Server) sheetWriter(ctx context.Context, uid int64, dest *store.Destination) (destination.Writer, error) {
	cfg, err := dest.SheetConfig()
	if err != nil {
		return nil, err
	}
	ts, err := s.userTokenSource(ctx, uid)
	if err != nil {
		return nil, err
	}
	// Validate (and, if needed, refresh) the token up front so a revoked grant
	// becomes a clean reconnect prompt rather than a mid-write failure.
	if _, err := ts.Token(); err != nil {
		s.markConnectionBroken(ctx, uid)
		return nil, errReconnect
	}
	return sheets.NewOAuthWriter(ctx, ts, cfg.SpreadsheetID, cfg.SheetTab, s.header(), s.sheetsOpts...)
}

// userTokenSource builds a refreshing token source for the user, persisting
// refreshed access tokens (re-encrypted) back to the store.
func (s *Server) userTokenSource(ctx context.Context, uid int64) (oauth2.TokenSource, error) {
	acct, err := s.store.GetOAuthAccount(ctx, uid, googleProvider)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errReconnect
	}
	if err != nil {
		return nil, err
	}
	if acct.Broken() {
		return nil, errReconnect
	}
	access, err := s.box.OpenString(acct.AccessTokenEnc)
	if err != nil {
		return nil, err
	}
	refresh := ""
	if len(acct.RefreshTokenEnc) > 0 {
		if refresh, err = s.box.OpenString(acct.RefreshTokenEnc); err != nil {
			return nil, err
		}
	}
	tok := &oauth2.Token{AccessToken: access, RefreshToken: refresh, Expiry: acct.TokenExpiry}
	return s.oauth.TokenSource(ctx, tok, func(newTok *oauth2.Token) {
		enc, e := s.box.SealString(newTok.AccessToken)
		if e != nil {
			return
		}
		if e := s.store.UpdateOAuthTokens(ctx, uid, googleProvider, enc, newTok.Expiry); e != nil {
			s.log.Error("persist refreshed token", "err", e)
		}
	}), nil
}

// markConnectionBroken flags both the OAuth link and the destination so
// /api/me and the destination picker prompt a reconnect.
func (s *Server) markConnectionBroken(ctx context.Context, uid int64) {
	if err := s.store.MarkOAuthBroken(ctx, uid, googleProvider); err != nil {
		s.log.Error("mark oauth broken", "err", err)
	}
	if err := s.store.MarkDestinationBroken(ctx, uid); err != nil {
		s.log.Error("mark destination broken", "err", err)
	}
}

// handleSheetsCreate auto-creates a new spreadsheet for the user (drive.file),
// writes the header row, and records it as the destination.
func (s *Server) handleSheetsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.googleEnabled() {
		httpError(w, http.StatusNotFound, "Google Sheets is not configured")
		return
	}
	uid := userID(r)
	ts, err := s.userTokenSource(r.Context(), uid)
	if err != nil {
		s.reconnectOrError(w, err)
		return
	}
	id, err := sheets.CreateSpreadsheet(r.Context(), ts, spreadsheetTitle, s.cfg.SheetTab, s.header(), s.sheetsOpts...)
	if err != nil {
		s.log.Error("create spreadsheet", "err", err)
		httpError(w, http.StatusBadGateway, "could not create your Google Sheet")
		return
	}
	if err := s.setSheetDestination(r.Context(), uid, &store.SheetConfig{
		SpreadsheetID: id, SheetTab: s.cfg.SheetTab, CreatedByApp: true,
	}); err != nil {
		s.log.Error("save sheet destination", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"spreadsheet_id": id, "sheet_tab": s.cfg.SheetTab, "created_by_app": true})
}

// handleSheetsAttach records an existing spreadsheet (chosen via the Google
// Picker) as the destination, resolving its first tab for appends.
func (s *Server) handleSheetsAttach(w http.ResponseWriter, r *http.Request) {
	if !s.googleEnabled() {
		httpError(w, http.StatusNotFound, "Google Sheets is not configured")
		return
	}
	var req struct {
		SpreadsheetID string `json:"spreadsheet_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SpreadsheetID == "" {
		httpError(w, http.StatusBadRequest, "spreadsheet_id is required")
		return
	}
	uid := userID(r)
	ts, err := s.userTokenSource(r.Context(), uid)
	if err != nil {
		s.reconnectOrError(w, err)
		return
	}
	tab, err := sheets.FirstSheetTitle(r.Context(), ts, req.SpreadsheetID, s.sheetsOpts...)
	if err != nil {
		// Surface the real Google error in the log while keeping the user message
		// friendly. 404 here means the server's stored token genuinely cannot see
		// the picked file (the symptom of a missing Picker setAppId); 403 is a
		// different permission problem.
		if gerr := (*googleapi.Error)(nil); errors.As(err, &gerr) {
			s.log.Error("attach spreadsheet", "code", gerr.Code, "gmsg", gerr.Message, "err", err)
		} else {
			s.log.Error("attach spreadsheet", "err", err)
		}
		httpError(w, http.StatusBadGateway, "could not open that spreadsheet")
		return
	}
	if err := s.setSheetDestination(r.Context(), uid, &store.SheetConfig{
		SpreadsheetID: req.SpreadsheetID, SheetTab: tab, CreatedByApp: false,
	}); err != nil {
		s.log.Error("save sheet destination", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"spreadsheet_id": req.SpreadsheetID, "sheet_tab": tab, "created_by_app": false})
}

// setSheetDestination makes a Google Sheet the user's active destination,
// replacing whatever was there before (one destination at a time).
func (s *Server) setSheetDestination(ctx context.Context, uid int64, cfg *store.SheetConfig) error {
	blob, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.store.SetDestination(ctx, &store.Destination{
		UserID: uid, Type: string(destination.TypeGoogleSheet), Config: blob,
	})
}

// reconnectOrError maps a token-source error to a response.
func (s *Server) reconnectOrError(w http.ResponseWriter, err error) {
	if errors.Is(err, errReconnect) {
		httpError(w, http.StatusConflict, "reconnect your Google account")
		return
	}
	s.log.Error("token source", "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}

// destinationConnected reports whether the user has a usable destination of
// any type. In dev / dry-run mode the in-memory writer is always "connected".
func (s *Server) destinationConnected(ctx context.Context, uid int64) bool {
	if !s.destinationsEnabled() {
		return true
	}
	dest, err := s.store.GetDestination(ctx, uid)
	return err == nil && !dest.Broken()
}
