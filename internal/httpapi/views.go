package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/store"
)

const previewRows = 3

// handleQueue lists submissions awaiting action (pending + failed_write),
// OLDEST FIRST — see store.ListQueue for why the queue inverts the ordering
// every other listing uses. Drives the top-bar badge, the queue list, and
// restoring an in-progress review after a reload.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListQueue(r.Context(), userID(r), 100)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	items := make([]submissionResponse, 0, len(subs))
	for i := range subs {
		items = append(items, s.submissionResponse(&subs[i], false))
	}

	resp := map[string]any{"count": len(items), "items": items}

	// "Captured while you were away": leads that arrived by email since the
	// user last opened this queue. Ingestion means leads now turn up
	// unattended, so the queue has to be able to say what it collected. A
	// failure here degrades to omitting the count, never to failing the queue.
	if s.cfg.EmailIngestConfigured() {
		uid := userID(r)
		if seen, err := s.store.QueueSeenAt(r.Context(), uid); err == nil && !seen.IsZero() {
			if n, err := s.store.CountCapturedSince(r.Context(), uid, seen); err == nil && n > 0 {
				resp["captured_while_away"] = n
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	rows, err := writer.Recent(r.Context(), previewRows)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"destinations": s.destinationList(r, userID(r)),
	})
}

// destinationList describes the user's active destination plus the other
// destination types they could switch to, for the picker. An alternative is
// disabled only when its provider is not configured on this server.
func (s *Server) destinationList(r *http.Request, uid int64) []map[string]any {
	active := s.activeDestination(r, uid)
	out := []map[string]any{active}

	activeType, _ := active["type"].(string)
	for _, alt := range []struct {
		typ     destination.Type
		label   string
		enabled bool
	}{
		{destination.TypeGoogleSheet, "Google Sheets", s.googleEnabled()},
		{destination.TypeNotion, "Notion", s.notionEnabled()},
	} {
		if string(alt.typ) == activeType {
			continue // already listed as the active destination
		}
		out = append(out, map[string]any{
			"id": string(alt.typ), "type": string(alt.typ), "label": alt.label,
			"disabled": !alt.enabled, "unavailable": !alt.enabled,
		})
	}
	return out
}

// activeDestination describes whichever destination the user currently writes
// to, or the not-yet-connected state.
func (s *Server) activeDestination(r *http.Request, uid int64) map[string]any {
	if !s.destinationsEnabled() {
		// Dev / dry-run: the in-memory sheet.
		dry := false
		if d, ok := s.writer.(dryRunner); ok && d.DryRun() {
			dry = true
		}
		return map[string]any{
			"id": "sheet", "type": "google_sheet", "active": true,
			"label": s.cfg.SheetTab + " (Google Sheet)", "dry_run": dry,
		}
	}

	dest, err := s.store.GetDestination(r.Context(), uid)
	if err != nil {
		// Authenticated but no destination yet (onboarding not finished).
		return map[string]any{
			"id": "sheet", "type": "google_sheet", "active": true,
			"label": "No destination connected", "connected": false, "needs_setup": true,
		}
	}

	out := map[string]any{"id": dest.Type, "type": dest.Type, "active": true}
	switch destination.Type(dest.Type) {
	case destination.TypeGoogleSheet:
		cfg, cerr := dest.SheetConfig()
		if cerr != nil {
			s.log.Error("decode sheet destination", "err", cerr)
			cfg = &store.SheetConfig{}
		}
		healthy := !dest.Broken() && s.googleConnected(r, uid)
		out["label"] = cfg.SheetTab + " (Google Sheet)"
		out["spreadsheet_id"] = cfg.SpreadsheetID
		out["created_by_app"] = cfg.CreatedByApp
		out["needs_reconnect"] = !healthy
		out["connected"] = healthy
	case destination.TypeNotion:
		cfg, cerr := dest.NotionConfig()
		if cerr != nil {
			s.log.Error("decode notion destination", "err", cerr)
			cfg = &store.NotionConfig{}
		}
		healthy := !dest.Broken() && s.notionConnected(r.Context(), uid)
		title := cfg.DatabaseTitle
		if title == "" {
			title = "Untitled"
		}
		out["label"] = title + " (Notion)"
		out["database_id"] = cfg.DatabaseID
		out["database_title"] = cfg.DatabaseTitle
		out["created_by_app"] = cfg.CreatedByApp
		out["needs_reconnect"] = !healthy
		out["connected"] = healthy
	default:
		out["label"] = dest.Type
		out["needs_reconnect"] = dest.Broken()
		out["connected"] = !dest.Broken()
	}
	return out
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
		// Source is the channel the lead arrived on, so history can show
		// where each row came from — not the schema's own "source" field,
		// which lives inside Result.
		Source      store.Source `json:"source,omitempty"`
		FromAddress string       `json:"from_address,omitempty"`
	}
	items := make([]item, 0, len(subs))
	for _, sub := range subs {
		items = append(items, item{
			ID: sub.ID, Excerpt: sub.InputExcerpt,
			Result: sub.Extraction, CreatedAt: sub.CreatedAt,
			Source: sub.Source, FromAddress: sub.FromAddress,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
