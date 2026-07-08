package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/EOEboh/sheetdrop/internal/extract"
	"github.com/EOEboh/sheetdrop/internal/llm"
	"github.com/EOEboh/sheetdrop/internal/store"
)

const maxImageBytes = 5 << 20 // 5 MB

var allowedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

type submitResponse struct {
	Status    store.Status `json:"status"`
	Duplicate bool         `json:"duplicate"`
	Result    *llm.Result  `json:"result,omitempty"`
	Flags     []string     `json:"flags,omitempty"`
	Message   string       `json:"message"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// parseSubmission reads the multipart form: a `text` field and/or an `image`
// file. Returns a user-facing error message when the input is invalid.
func parseSubmission(r *http.Request) (text string, image []byte, mediaType string, errMsg string) {
	if err := r.ParseMultipartForm(maxImageBytes + 1<<20); err != nil {
		return "", nil, "", "invalid multipart form"
	}
	text = strings.TrimSpace(r.FormValue("text"))

	file, header, err := r.FormFile("image")
	switch {
	case errors.Is(err, http.ErrMissingFile):
		// text-only submission
	case err != nil:
		return "", nil, "", "could not read image upload"
	default:
		defer file.Close()
		if header.Size > maxImageBytes {
			return "", nil, "", "image exceeds the 5 MB limit"
		}
		image, err = io.ReadAll(io.LimitReader(file, maxImageBytes+1))
		if err != nil || int64(len(image)) > maxImageBytes {
			return "", nil, "", "image exceeds the 5 MB limit"
		}
		mediaType = http.DetectContentType(image)
		if !allowedImageTypes[mediaType] {
			return "", nil, "", fmt.Sprintf("unsupported image type %s (use png, jpeg, webp, or gif)", mediaType)
		}
	}

	if text == "" && len(image) == 0 {
		return "", nil, "", "submit some text or an image"
	}
	return text, image, mediaType, ""
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	text, image, mediaType, errMsg := parseSubmission(r)
	if errMsg != "" {
		httpError(w, http.StatusBadRequest, errMsg)
		return
	}

	now := time.Now().UTC()
	hash := store.ContentHash(text, image, now)
	log := s.log.With("hash", hash[:12])

	// Idempotency: an identical submission today returns the prior outcome
	// without another LLM call or sheet row.
	if prior, err := s.store.FindByHash(ctx, hash); err != nil {
		log.Error("dedup lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	} else if prior != nil {
		writeJSON(w, http.StatusOK, priorResponse(prior))
		return
	}

	result, err := s.extractor.Extract(ctx, llm.Input{
		Text:           text,
		Image:          image,
		ImageMediaType: mediaType,
		SubmissionDate: now,
	})
	if err != nil {
		log.Error("extraction failed", "err", err)
		httpError(w, http.StatusBadGateway, "extraction failed — please try again")
		return
	}

	verdict := extract.Validate(result, s.cfg.Schema.RequiredFields, now)
	resultJSON, _ := json.Marshal(result)

	resp := submitResponse{Result: result, Flags: verdict.Flags}
	sub := &store.Submission{
		ContentHash:  hash,
		Extraction:   resultJSON,
		InputExcerpt: excerpt(text, image),
	}

	if verdict.NeedsReview {
		sub.Status = store.StatusNeedsReview
		resp.Status = store.StatusNeedsReview
		resp.Flags = append(verdict.Reasons, verdict.Flags...)
		resp.Message = "Not written to the sheet — flagged for review: " + strings.Join(verdict.Reasons, "; ")
	} else if err := s.writer.Append(ctx, s.buildRow(result, verdict.Flags)); err != nil {
		log.Error("sheet write failed", "err", err)
		sub.Status = store.StatusFailedWrite
		sub.Error = err.Error()
		resp.Status = store.StatusFailedWrite
		resp.Message = "Extraction succeeded but writing to Google Sheets failed. The result is saved locally in the failed-writes queue."
	} else {
		sub.Status = store.StatusWritten
		resp.Status = store.StatusWritten
		resp.Message = "Lead added to your sheet."
		if len(verdict.Flags) > 0 {
			resp.Message += " Note: " + strings.Join(verdict.Flags, "; ")
		}
	}

	flagsJSON, _ := json.Marshal(resp.Flags)
	sub.Flags = flagsJSON
	if _, err := s.store.Insert(ctx, sub); err != nil {
		log.Error("store insert failed", "err", err)
	}

	log.Info("submission processed",
		"status", sub.Status,
		"confidence", result.Confidence,
		"missing_fields", result.MissingFields,
		"multiple_leads", result.MultipleLeadsDetected,
		"has_image", len(image) > 0,
	)
	writeJSON(w, http.StatusOK, resp)
}

// buildRow maps the result to sheet columns per config — no field names
// hardcoded. The synthetic "flags" column carries review-worthy notices.
func (s *Server) buildRow(res *llm.Result, flags []string) []string {
	row := make([]string, 0, len(s.cfg.Schema.Columns))
	for _, col := range s.cfg.Schema.Columns {
		if col == "flags" {
			row = append(row, strings.Join(flags, "; "))
			continue
		}
		val, _ := res.Field(col)
		row = append(row, val)
	}
	return row
}

func priorResponse(prior *store.Submission) submitResponse {
	resp := submitResponse{
		Status:    prior.Status,
		Duplicate: true,
		Message:   "This content was already submitted today — no new row was created.",
	}
	if len(prior.Extraction) > 0 {
		var res llm.Result
		if json.Unmarshal(prior.Extraction, &res) == nil {
			resp.Result = &res
		}
	}
	if len(prior.Flags) > 0 {
		json.Unmarshal(prior.Flags, &resp.Flags)
	}
	return resp
}

// excerpt keeps a short, non-sensitive preview for the review queues.
func excerpt(text string, image []byte) string {
	const max = 120
	t := strings.TrimSpace(text)
	if t == "" {
		return fmt.Sprintf("[image submission, %d bytes]", len(image))
	}
	if len(t) > max {
		t = t[:max] + "…"
	}
	return t
}

func (s *Server) handleQueue(status store.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subs, err := s.store.ListByStatus(r.Context(), status, 100)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		type item struct {
			ID        int64           `json:"id"`
			Excerpt   string          `json:"excerpt"`
			Result    json.RawMessage `json:"result,omitempty"`
			Flags     json.RawMessage `json:"flags,omitempty"`
			Error     string          `json:"error,omitempty"`
			CreatedAt time.Time       `json:"created_at"`
		}
		items := make([]item, 0, len(subs))
		for _, sub := range subs {
			items = append(items, item{
				ID: sub.ID, Excerpt: sub.InputExcerpt,
				Result: sub.Extraction, Flags: sub.Flags,
				Error: sub.Error, CreatedAt: sub.CreatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "items": items})
	}
}
