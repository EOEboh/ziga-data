package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/ingest"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
)

// quarantineItem is one filtered message as the UI renders it.
type quarantineItem struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	// Reason is the machine-readable category; the frontend maps it to
	// wording, so the two can change independently.
	Reason string `json:"reason"`
	// Detail names the exact rule that fired, shown on request. It is what
	// makes "why was this filtered?" answerable without reading logs.
	Detail      string    `json:"detail,omitempty"`
	FromAddress string    `json:"from_address,omitempty"`
	FromName    string    `json:"from_name,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Excerpt     string    `json:"excerpt"`
	ReceivedAt  time.Time `json:"received_at"`
	// Rescuable reports whether there is still a body to re-extract. Retention
	// purges bodies, so an old item can be dismissed but not rescued — the UI
	// must not offer an action that cannot work.
	Rescuable bool `json:"rescuable"`

	// VerifyCode / VerifyURL are the forwarding handshake, set on
	// verification items.
	VerifyCode string `json:"verify_code,omitempty"`
	VerifyURL  string `json:"verify_url,omitempty"`
}

type quarantineResponse struct {
	Items []quarantineItem `json:"items"`
}

func toQuarantineItem(ev store.IngestionEvent) quarantineItem {
	return quarantineItem{
		ID:          ev.ID,
		Status:      string(ev.Status),
		Reason:      ev.Reason,
		Detail:      ev.Detail,
		FromAddress: ev.FromAddress,
		FromName:    ev.FromName,
		Subject:     ev.Subject,
		Excerpt:     ev.BodyExcerpt,
		ReceivedAt:  ev.ReceivedAt,
		Rescuable:   strings.TrimSpace(ev.BodyText) != "",
		VerifyCode:  ev.VerifyCode,
		VerifyURL:   ev.VerifyURL,
	}
}

// handleQuarantine lists the user's filtered mail.
//
// This view is what makes "a lead is never silently lost" a property the user
// can check rather than a promise. ?status=verification narrows it to pending
// forwarding handshakes, which the setup screen polls for.
func (s *Server) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	statuses := []store.EventStatus{store.EventQuarantined}
	if r.URL.Query().Get("status") == "verification" {
		statuses = []store.EventStatus{store.EventVerification}
	}

	events, err := s.store.ListEvents(r.Context(), uid, statuses, 200)
	if err != nil {
		s.log.Error("quarantine: list failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	items := make([]quarantineItem, 0, len(events))
	for _, ev := range events {
		items = append(items, toQuarantineItem(ev))
	}
	writeJSON(w, http.StatusOK, quarantineResponse{Items: items})
}

// handleQuarantineRescue re-runs extraction on a filtered message and puts the
// result in the review queue. This is the escape hatch for every judgement
// call the filters make: a "support@" address that is really a person, a short
// message that really is a lead.
func (s *Server) handleQuarantineRescue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}

	ev, err := s.store.GetEvent(ctx, uid, id)
	if err != nil {
		s.log.Error("quarantine: lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Another tenant's event reads as absent, so ids stay non-enumerable.
	if ev == nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if strings.TrimSpace(ev.BodyText) == "" {
		// Retention purged the body. Say so plainly rather than producing an
		// empty lead the user then has to work out the cause of.
		httpError(w, http.StatusGone, "This message is too old to rescue — its contents have been cleared")
		return
	}

	// A rescue is a deliberate, explicit act by the user, so it bypasses the
	// filters — they are what put it here — but not the extraction budget,
	// which exists to bound spend regardless of how a message arrived.
	if over, why := s.overIngestCap(ctx, uid, time.Now().UTC()); over {
		s.log.Warn("quarantine: rescue blocked by cap", "user_id", uid, "detail", why)
		httpError(w, http.StatusTooManyRequests, "You've hit today's capture limit. Try again tomorrow")
		return
	}

	forwarded := ev.FromAddress != ""
	out, err := s.ingestLead(ctx, leadInput{
		UserID:      uid,
		Text:        ev.BodyText,
		Now:         time.Now().UTC(),
		Source:      store.SourceEmail,
		MessageID:   ev.MessageID,
		FromAddress: ev.FromAddress,
		Subject:     ev.Subject,
		ReceivedAt:  ev.ReceivedAt,
		Email: &llm.EmailMeta{
			From:       ev.FromAddress,
			FromName:   ev.FromName,
			Subject:    ev.Subject,
			Forwarded:  forwarded,
			ReceivedAt: ev.ReceivedAt,
		},
	})
	if err != nil {
		s.log.Error("quarantine: rescue extraction failed", "err", err)
		httpError(w, http.StatusBadGateway, "Extraction failed. Try again")
		return
	}

	if err := s.store.SettleEvent(ctx, uid, id, store.EventRescued, out.Submission.ID); err != nil {
		// The lead exists, which is what matters; the event row just did not
		// move. Log it rather than telling the user the rescue failed and
		// having them do it twice.
		s.log.Error("quarantine: rescued but could not settle the event", "event_id", id, "err", err)
	}
	s.log.Info("quarantine: rescued", "user_id", uid, "event_id", id, "submission_id", out.Submission.ID)
	writeJSON(w, http.StatusOK, s.submissionResponse(out.Submission, out.Duplicate))
}

// handleQuarantineDismiss closes a filtered message without extracting it.
func (s *Server) handleQuarantineDismiss(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.SettleEvent(r.Context(), uid, id, store.EventDismissed, 0); err != nil {
		// Not found covers both "another tenant's row" and "already settled".
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

type blockSenderRequest struct {
	Pattern string `json:"pattern"`
}

// handleBlockSender adds a sender to the user's blocklist.
func (s *Server) handleBlockSender(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var req blockSenderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	pattern := strings.ToLower(strings.TrimSpace(req.Pattern))
	if pattern == "" {
		httpError(w, http.StatusBadRequest, "give an address or @domain to block")
		return
	}
	// Both forms must actually be able to match something. A pattern that
	// matches nothing is worse than no pattern at all: the user believes they
	// have blocked a sender and keeps receiving them.
	const badPattern = "block a full address (name@example.com) or a domain (@example.com)"
	if strings.HasPrefix(pattern, "@") {
		if !strings.Contains(strings.TrimPrefix(pattern, "@"), ".") {
			httpError(w, http.StatusBadRequest, badPattern)
			return
		}
	} else if _, domain, ok := ingest.SplitAddress(pattern); !ok || !strings.Contains(domain, ".") {
		httpError(w, http.StatusBadRequest, badPattern)
		return
	}

	// Blocking the forwarding-confirmation sender would let a user break their
	// own setup permanently with one click, and the cause would be invisible:
	// they would simply never receive another confirmation code.
	if ingest.IsVerificationSender(pattern) {
		httpError(w, http.StatusBadRequest,
			"That address delivers your forwarding confirmation codes, so it can't be blocked")
		return
	}

	if err := s.store.BlockSender(r.Context(), uid, pattern); err != nil {
		s.log.Error("block sender failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "blocked", "pattern": pattern})
}

// handleBlockedSenders lists the user's blocklist.
func (s *Server) handleBlockedSenders(w http.ResponseWriter, r *http.Request) {
	patterns, err := s.store.BlockedSenders(r.Context(), userID(r))
	if err != nil {
		s.log.Error("blocked senders list failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if patterns == nil {
		patterns = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"patterns": patterns})
}

// handleUnblockSender removes a pattern from the user's blocklist.
func (s *Server) handleUnblockSender(w http.ResponseWriter, r *http.Request) {
	var req blockSenderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := s.store.UnblockSender(r.Context(), userID(r), req.Pattern); err != nil {
		s.log.Error("unblock sender failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unblocked"})
}

// handleQueueSeen records that the user has looked at their review queue,
// which is what the "captured while you were away" count measures from.
func (s *Server) handleQueueSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MarkQueueSeen(r.Context(), userID(r), time.Now().UTC()); err != nil {
		s.log.Error("mark queue seen failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
