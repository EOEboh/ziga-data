package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/store"
)

const previewRows = 3

// handleQueue lists submissions awaiting action (pending + failed_write),
// newest first. Drives the top-bar badge and restores in-progress reviews
// after a reload.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListByStatuses(r.Context(), userID(r),
		[]store.Status{store.StatusPending, store.StatusFailedWrite}, 100)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	items := make([]submissionResponse, 0, len(subs))
	for i := range subs {
		items = append(items, s.submissionResponse(&subs[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

// handlePreview returns the last rows of the connected sheet for the preview
// strip. Sheet errors degrade to an empty strip rather than failing the page.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	cols := s.cfg.Schema.Columns
	writer, err := s.writerFor(r.Context(), userID(r))
	if err != nil {
		// No sheet / needs reconnect: an empty preview, not a page error.
		writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": [][]string{}})
		return
	}
	rows, err := writer.LastRows(r.Context(), previewRows)
	if err != nil {
		s.log.Error("preview read failed", "err", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"columns": cols, "rows": [][]string{}, "error": "preview unavailable",
		})
		return
	}
	// The Sheets API trims trailing empty cells; pad so every row has one
	// cell per column.
	padded := make([][]string, len(rows))
	for i, row := range rows {
		p := make([]string, len(cols))
		copy(p, row)
		padded[i] = p
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": padded})
}

// dryRunner is implemented by the in-memory dev writer; a real sheets client
// doesn't have it.
type dryRunner interface{ DryRun() bool }

func (s *Server) handleDestination(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	active := map[string]any{"id": "sheet", "type": "google_sheet", "active": true}

	if !s.googleEnabled() {
		// Dev / dry-run: the in-memory sheet.
		dry := false
		if d, ok := s.writer.(dryRunner); ok && d.DryRun() {
			dry = true
		}
		active["label"] = s.cfg.SheetTab + " (Google Sheet)"
		active["dry_run"] = dry
	} else if sheet, err := s.store.GetUserSheet(r.Context(), uid); err == nil {
		active["label"] = sheet.SheetTab + " (Google Sheet)"
		active["spreadsheet_id"] = sheet.SpreadsheetID
		active["created_by_app"] = sheet.CreatedByApp
		active["needs_reconnect"] = sheet.Broken() || !s.googleConnected(r, uid)
		active["connected"] = !sheet.Broken() && s.googleConnected(r, uid)
	} else {
		// Authenticated but no destination yet (onboarding not finished).
		active["label"] = "No sheet connected"
		active["connected"] = false
		active["needs_setup"] = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"destinations": []map[string]any{
			active,
			{"id": "notion", "label": "Notion", "type": "notion", "disabled": true, "coming_soon": true},
		},
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListByStatus(r.Context(), userID(r), store.StatusWritten, 50)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	type item struct {
		ID        int64           `json:"id"`
		Excerpt   string          `json:"excerpt"`
		Result    json.RawMessage `json:"result,omitempty"`
		CreatedAt time.Time       `json:"created_at"`
	}
	items := make([]item, 0, len(subs))
	for _, sub := range subs {
		items = append(items, item{
			ID: sub.ID, Excerpt: sub.InputExcerpt,
			Result: sub.Extraction, CreatedAt: sub.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
