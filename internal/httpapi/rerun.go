package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
)

// handleRerun re-extracts an existing submission from corrected input, and
// replaces it.
//
// This exists because the obvious client-side implementation — submit the
// edited text as a new paste, then discard the original — silently destroys
// everything the submission knew about itself. For an email-captured lead that
// means the sender address, which for a forwarded message is the only record
// of who the lead actually is, and the entire point of the attribution work in
// internal/ingest.
//
// So provenance is carried here, on the server, read from the stored row. It
// is never accepted from the client: a browser able to assert from_address
// could forge "this lead came from someone@bigcorp.com" on any paste.
func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	orig, err := s.store.Get(ctx, uid, id)
	if err != nil {
		s.log.Error("rerun: lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Another tenant's submission reads as absent, so ids stay non-enumerable.
	if orig == nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if orig.Status == store.StatusWritten || orig.Status == store.StatusDiscarded {
		// Re-running something already settled would resurrect a lead the user
		// has finished with, and for a written one would invite a duplicate row
		// at the destination.
		httpError(w, http.StatusConflict, "This lead has already been settled and can't be re-run")
		return
	}

	text, image, mediaType, errMsg := parseSubmission(r)
	if errMsg != "" {
		httpError(w, http.StatusBadRequest, errMsg)
		return
	}

	in := leadInput{
		UserID:    uid,
		Text:      text,
		Image:     image,
		ImageType: mediaType,
		Now:       time.Now().UTC(),
		// Everything below is inherited, not submitted.
		Source:      orig.Source,
		MessageID:   orig.MessageID,
		FromAddress: orig.FromAddress,
		Subject:     orig.Subject,
		ReceivedAt:  orig.ReceivedAt,
		ReplacesID:  id,
	}
	if orig.Source == store.SourceEmail {
		// The model is told this came from email for the same reason it was on
		// the first pass: without it, a forwarded body reads as though the
		// forwarder is the lead.
		in.Email = &llm.EmailMeta{
			From:       orig.FromAddress,
			Subject:    orig.Subject,
			Forwarded:  orig.FromAddress != "",
			ReceivedAt: orig.ReceivedAt,
		}
	}

	out, err := s.ingestLead(ctx, in)
	if err != nil {
		// The original is untouched on failure — it is only discarded once a
		// replacement exists. A transient model outage must not cost the user
		// the lead they were trying to correct.
		switch {
		case errors.Is(err, errExtractionFailed):
			s.log.Error("rerun: extraction failed", "id", id, "err", err)
			httpError(w, http.StatusBadGateway, "Extraction failed. Try again")
		default:
			s.log.Error("rerun: failed", "id", id, "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Unedited input dedups straight back onto the original, in which case
	// there is nothing to replace and discarding would destroy the very row
	// being returned.
	if out.Submission.ID != id {
		if err := s.store.Discard(ctx, uid, id); err != nil {
			// The replacement exists, which is what the user asked for. Leaving
			// the original visible is a duplicate in the queue, not a loss, so
			// this is logged rather than surfaced as a failure they would retry.
			s.log.Error("rerun: replacement created but the original was not discarded",
				"original", id, "replacement", out.Submission.ID, "err", err)
		}
	}

	s.log.Info("rerun: re-extracted",
		"original", id, "replacement", out.Submission.ID, "source", orig.Source)
	writeJSON(w, http.StatusOK, s.submissionResponse(out.Submission, out.Duplicate))
}
