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

// submissionResponse is the shared JSON shape for a submission: returned by
// POST /api/submit and as the items of GET /api/queue, so the review pane
// renders fresh extractions and reloaded queue items identically.
type submissionResponse struct {
	ID          int64                         `json:"id"`
	Status      store.Status                  `json:"status"`
	Duplicate   bool                          `json:"duplicate,omitempty"`
	Result      *llm.Result                   `json:"result,omitempty"`
	FieldStates map[string]extract.FieldState `json:"field_states,omitempty"`
	Flags       []string                      `json:"flags,omitempty"`
	Error       string                        `json:"error,omitempty"`
	Input       submissionInput               `json:"input"`
	CreatedAt   time.Time                     `json:"created_at"`
}

type submissionInput struct {
	Text     string `json:"text,omitempty"`
	HasImage bool   `json:"has_image"`
	ImageURL string `json:"image_url,omitempty"`
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

// handleSubmit extracts a lead and stores it as pending. Nothing is written
// to the sheet here — that only happens on an explicit confirm.
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
	// without another LLM call.
	if prior, err := s.store.FindByHash(ctx, hash); err != nil {
		log.Error("dedup lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	} else if prior != nil {
		writeJSON(w, http.StatusOK, s.submissionResponse(prior, true))
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
		httpError(w, http.StatusBadGateway, "Extraction failed. Try again")
		return
	}

	verdict := extract.Validate(result, s.cfg.Schema, now)
	resultJSON, _ := json.Marshal(result)
	flagsJSON, _ := json.Marshal(verdict.Flags)

	sub := &store.Submission{
		ContentHash:    hash,
		Status:         store.StatusPending,
		Extraction:     resultJSON,
		Flags:          flagsJSON,
		InputExcerpt:   excerpt(text, image),
		InputText:      text,
		InputImage:     image,
		InputImageType: mediaType,
	}
	duplicate, err := s.store.Insert(ctx, sub)
	if err != nil {
		log.Error("store insert failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if duplicate {
		// Lost an insert race with an identical concurrent submission.
		if prior, err := s.store.FindByHash(ctx, hash); err == nil && prior != nil {
			writeJSON(w, http.StatusOK, s.submissionResponse(prior, true))
			return
		}
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}

	log.Info("submission extracted",
		"id", sub.ID,
		"needs_attention", verdict.NeedsAttention,
		"confidence", result.Confidence,
		"multiple_leads", result.MultipleLeadsDetected,
		"has_image", len(image) > 0,
	)
	writeJSON(w, http.StatusOK, s.submissionResponse(sub, false))
}

// submissionResponse builds the shared response shape from a stored
// submission, recomputing per-field states from the extraction blob.
func (s *Server) submissionResponse(sub *store.Submission, duplicate bool) submissionResponse {
	resp := submissionResponse{
		ID:        sub.ID,
		Status:    sub.Status,
		Duplicate: duplicate,
		Error:     sub.Error,
		CreatedAt: sub.CreatedAt,
		Input: submissionInput{
			Text:     sub.InputText,
			HasImage: len(sub.InputImage) > 0,
		},
	}
	if resp.Input.HasImage {
		resp.Input.ImageURL = fmt.Sprintf("/api/submissions/%d/image", sub.ID)
	}
	if resp.Input.Text == "" && !resp.Input.HasImage {
		// Rows stored before full input was kept: the excerpt is all we have.
		resp.Input.Text = sub.InputExcerpt
	}
	if len(sub.Extraction) > 0 {
		var res llm.Result
		if json.Unmarshal(sub.Extraction, &res) == nil {
			resp.Result = &res
			v := extract.Validate(&res, s.cfg.Schema, sub.CreatedAt)
			resp.FieldStates = v.Fields
		}
	}
	if len(sub.Flags) > 0 {
		json.Unmarshal(sub.Flags, &resp.Flags)
	}
	return resp
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

// excerpt keeps a short preview for queue and history listings.
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
