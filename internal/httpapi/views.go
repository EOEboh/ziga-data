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
	subs, err := s.store.ListByStatuses(r.Context(),
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
	rows, err := s.writer.LastRows(r.Context(), previewRows)
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
	label := s.cfg.SheetTab + " (Google Sheet)"
	dry := false
	if d, ok := s.writer.(dryRunner); ok && d.DryRun() {
		dry = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"destinations": []map[string]any{
			{"id": "sheet", "label": label, "type": "google_sheet", "active": true, "dry_run": dry},
			{"id": "notion", "label": "Notion", "type": "notion", "disabled": true, "coming_soon": true},
		},
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListByStatus(r.Context(), store.StatusWritten, 50)
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
