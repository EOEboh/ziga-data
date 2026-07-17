package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EOEboh/sheetdrop/internal/extract"
	"github.com/EOEboh/sheetdrop/internal/llm"
	"github.com/EOEboh/sheetdrop/internal/store"
)

type confirmRequest struct {
	// Fields carries the (possibly edited) values from the review pane,
	// keyed by schema field name. Only provided keys are applied.
	Fields map[string]string `json:"fields"`
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpError(w, http.StatusBadRequest, "invalid submission id")
		return 0, false
	}
	return id, true
}

// handleConfirm writes a reviewed submission to the sheet. This is the only
// code path that appends a row. Retrying a failed write is the same call —
// failed_write submissions are accepted alongside pending ones.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req confirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// One confirm at a time: two concurrent confirms of the same submission
	// would append two sheet rows. The status check below runs inside the
	// lock, so the second click gets a 409 instead of a duplicate row.
	s.confirmMu.Lock()
	defer s.confirmMu.Unlock()

	ctx := r.Context()
	sub, err := s.store.Get(ctx, id)
	if err != nil {
		s.log.Error("confirm lookup failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if sub == nil {
		httpError(w, http.StatusNotFound, "submission not found")
		return
	}
	if sub.Status == store.StatusWritten {
		httpError(w, http.StatusConflict, "already written to the sheet")
		return
	}

	var res llm.Result
	if err := json.Unmarshal(sub.Extraction, &res); err != nil {
		s.log.Error("stored extraction unreadable", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for name, val := range req.Fields {
		if !res.SetField(name, strings.TrimSpace(val)) {
			httpError(w, http.StatusBadRequest, "unknown field "+strconv.Quote(name))
			return
		}
	}

	// Required fields must be filled before anything is written. Low
	// confidence no longer blocks — the user has reviewed the values.
	verdict := extract.Validate(&res, s.cfg.Schema, time.Now().UTC())
	for _, state := range verdict.Fields {
		if state == extract.FieldMissing {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":        "required fields missing",
				"field_states": verdict.Fields,
			})
			return
		}
	}

	mergedJSON, _ := json.Marshal(&res)
	var flags []string
	if len(sub.Flags) > 0 {
		json.Unmarshal(sub.Flags, &flags)
	}

	if err := s.writer.Append(ctx, s.buildRow(&res, flags)); err != nil {
		s.log.Error("sheet write failed", "id", id, "err", err)
		if uerr := s.store.Update(ctx, id, store.StatusFailedWrite, mergedJSON, err.Error()); uerr != nil {
			s.log.Error("store update failed", "id", id, "err", uerr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"id":     id,
			"status": store.StatusFailedWrite,
			"error":  "Could not write to your sheet. Retry",
		})
		return
	}
	if err := s.store.Update(ctx, id, store.StatusWritten, mergedJSON, ""); err != nil {
		// The row is on the sheet; only local bookkeeping failed. Surface
		// success — a retry here would duplicate the sheet row.
		s.log.Error("store update failed after sheet write", "id", id, "err", err)
	}
	s.log.Info("submission confirmed", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": store.StatusWritten})
}

func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sub, err := s.store.Get(ctx, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if sub == nil {
		httpError(w, http.StatusNotFound, "submission not found")
		return
	}
	if sub.Status == store.StatusWritten {
		httpError(w, http.StatusConflict, "already written to the sheet")
		return
	}
	if err := s.store.Delete(ctx, id); err != nil {
		s.log.Error("discard failed", "id", id, "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Info("submission discarded", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "discarded"})
}

// handleImage serves the original uploaded image for the review pane's
// left panel (needed when a queued item is reloaded after a refresh).
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	sub, err := s.store.Get(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if sub == nil || len(sub.InputImage) == 0 {
		httpError(w, http.StatusNotFound, "no image for this submission")
		return
	}
	w.Header().Set("Content-Type", sub.InputImageType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(sub.InputImage)
}
