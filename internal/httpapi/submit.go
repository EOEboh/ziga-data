package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/extract"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
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

	// Source is the channel this lead arrived on. Distinct from the schema's
	// own "source" field inside Result, which is what the lead says about
	// where THEY came from.
	Source store.Source `json:"source,omitempty"`
	// Set for email-sourced leads, so the review pane can show where it came
	// from. With forwarding involved, FromAddress is the original
	// correspondent rather than whoever forwarded it.
	FromAddress string     `json:"from_address,omitempty"`
	Subject     string     `json:"subject,omitempty"`
	ReceivedAt  *time.Time `json:"received_at,omitempty"`
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

// handleSubmit extracts a pasted lead and stores it as pending. Nothing is
// written to the destination here — that only happens on an explicit confirm.
//
// The work happens in ingestLead, shared with the email ingestion path; this
// handler is the paste-specific shell around it: parse the multipart form, and
// map failures onto HTTP.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(r)
	text, image, mediaType, errMsg := parseSubmission(r)
	if errMsg != "" {
		httpError(w, http.StatusBadRequest, errMsg)
		return
	}

	now := time.Now().UTC()
	log := s.log.With("hash", store.ContentHash(uid, text, image, now)[:12])

	out, err := s.ingestLead(ctx, leadInput{
		UserID:    uid,
		Text:      text,
		Image:     image,
		ImageType: mediaType,
		Now:       now,
		Source:    store.SourcePaste,
	})
	if err != nil {
		switch {
		case errors.Is(err, errExtractionFailed):
			log.Error("extraction failed", "err", err)
			httpError(w, http.StatusBadGateway, "Extraction failed. Try again")
		default:
			log.Error("submission failed", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if !out.Duplicate {
		log.Info("submission extracted",
			"id", out.Submission.ID,
			"needs_attention", out.Verdict.NeedsAttention,
			"has_image", len(image) > 0,
		)
	}
	writeJSON(w, http.StatusOK, s.submissionResponse(out.Submission, out.Duplicate))
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
		Source:      sub.Source,
		FromAddress: sub.FromAddress,
		Subject:     sub.Subject,
	}
	if !sub.ReceivedAt.IsZero() {
		received := sub.ReceivedAt
		resp.ReceivedAt = &received
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

// buildLead maps the result to the schema's columns per config — no field
// names hardcoded. The synthetic "flags" column carries review-worthy notices.
//
// Cells stay in column order, which is the row a sheet writes directly; each
// cell also carries its field name, which is what a property-oriented
// destination like Notion maps on.
func (s *Server) buildLead(res *llm.Result, flags []string) destination.Lead {
	cells := make([]destination.Cell, 0, len(s.cfg.Schema.Columns))
	for _, col := range s.cfg.Schema.Columns {
		if col == "flags" {
			cells = append(cells, destination.Cell{Field: col, Value: strings.Join(flags, "; ")})
			continue
		}
		val, _ := res.Field(col)
		cells = append(cells, destination.Cell{Field: col, Value: val})
	}
	return destination.Lead{Cells: cells}
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
