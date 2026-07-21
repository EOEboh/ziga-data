package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/EOEboh/ziga-data/internal/sheets"
	"github.com/EOEboh/ziga-data/internal/store"
	"golang.org/x/oauth2"
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

// header is the column-name row to maintain, or nil in no-header mode.
func (s *Server) header() []string {
	if s.cfg.HeaderRow {
		return s.cfg.Schema.Columns
	}
	return nil
}

// writerFor resolves the RowWriter for a user. In dev/dry-run it's the shared
// in-memory writer; otherwise it's a per-user Sheets writer built from the
// user's connected sheet and OAuth token. Returns errNoSheet / errReconnect for
// the onboarding and reconnect states.
func (s *Server) writerFor(ctx context.Context, uid int64) (RowWriter, error) {
	if !s.googleEnabled() {
		return s.writer, nil
	}
	sheet, err := s.store.GetUserSheet(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNoSheet
	}
	if err != nil {
		return nil, err
	}
	if sheet.Broken() {
		return nil, errReconnect
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
	return sheets.NewOAuthWriter(ctx, ts, sheet.SpreadsheetID, sheet.SheetTab, s.header(), s.sheetsOpts...)
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

// markConnectionBroken flags both the OAuth link and the sheet so /api/me and
// the destination picker prompt a reconnect.
func (s *Server) markConnectionBroken(ctx context.Context, uid int64) {
	if err := s.store.MarkOAuthBroken(ctx, uid, googleProvider); err != nil {
		s.log.Error("mark oauth broken", "err", err)
	}
	if err := s.store.MarkSheetBroken(ctx, uid); err != nil {
		s.log.Error("mark sheet broken", "err", err)
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
	if err := s.store.SetUserSheet(r.Context(), &store.UserSheet{
		UserID: uid, SpreadsheetID: id, SheetTab: s.cfg.SheetTab, CreatedByApp: true,
	}); err != nil {
		s.log.Error("save user sheet", "err", err)
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
		s.log.Error("attach spreadsheet", "err", err)
		httpError(w, http.StatusBadGateway, "could not open that spreadsheet")
		return
	}
	if err := s.store.SetUserSheet(r.Context(), &store.UserSheet{
		UserID: uid, SpreadsheetID: req.SpreadsheetID, SheetTab: tab, CreatedByApp: false,
	}); err != nil {
		s.log.Error("save user sheet", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"spreadsheet_id": req.SpreadsheetID, "sheet_tab": tab, "created_by_app": false})
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

// sheetConnected reports whether the user has a usable destination. In dev /
// dry-run mode the in-memory writer is always "connected".
func (s *Server) sheetConnected(ctx context.Context, uid int64) bool {
	if !s.googleEnabled() {
		return true
	}
	sheet, err := s.store.GetUserSheet(ctx, uid)
	return err == nil && !sheet.Broken()
}
