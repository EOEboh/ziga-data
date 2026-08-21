package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EOEboh/ziga-data/internal/ingest"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
)

// maxIngestBytes caps the ingestion request body. v1 extracts text only, and
// the worker already refuses anything larger, so this is defence in depth
// against a body that never came from our worker.
const maxIngestBytes = 2 << 20 // 2 MB

// Ingestion outcomes, reported back to the worker so it knows whether to
// retry. Every one of these means "do not retry"; a retry is signalled by a
// 5xx status instead.
const (
	ingestAccepted     = "accepted"
	ingestDuplicate    = "duplicate"
	ingestQuarantined  = "quarantined"
	ingestVerification = "verification"
	ingestDiscarded    = "discarded"
)

type ingestResponse struct {
	Status string `json:"status"`
	ID     int64  `json:"id,omitempty"`
}

// handleEmailIngest accepts one inbound email from the mail worker.
//
// It is registered bare — no CSRF, no session — because the caller is a mail
// worker rather than a browser. Authentication is an HMAC over the exact
// request body, so the body must be read before anything decodes it.
func (s *Server) handleEmailIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()

	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.log.Warn("ingest: unreadable body", "err", err)
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}

	ok, reason := ingest.VerifySignature([]byte(s.cfg.IngestSharedSecret),
		r.Header.Get(ingest.TimestampHeader), r.Header.Get(ingest.SignatureHeader), raw, now)
	if !ok {
		// The reason is logged, never returned: a caller able to tell a bad
		// signature from a stale timestamp learns more than it should.
		s.log.Warn("ingest: rejected unauthenticated request", "reason", reason)
		httpError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var msg ingest.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		s.log.Warn("ingest: undecodable payload", "err", err)
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}

	// Address-keyed burst limit, applied before the database lookup so probing
	// for live addresses is throttled at the same rate as real mail and cannot
	// be used to enumerate them faster than delivery.
	addrKey := "addr:" + strings.ToLower(strings.TrimSpace(msg.To))
	if !s.ingestLimiter.get(addrKey).Allow() {
		s.log.Warn("ingest: address rate limited", "to", msg.To)
		httpError(w, http.StatusTooManyRequests, "slow down")
		return
	}

	addr, uid, resolved := s.resolveInbound(r, msg.To)
	if !resolved {
		// Deliberately uninformative and identical to a filtered message: an
		// unknown address must not be distinguishable from a known one. There
		// is no event row here because an event belongs to a tenant and this
		// mail has none.
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestDiscarded})
		return
	}
	log := s.log.With("user_id", uid, "message_id", msg.MessageID, "address_id", addr.ID)

	if over, why := s.overIngestCap(ctx, uid, now); over {
		// The user, not the worker, is the one who has to act here (their
		// forwarding rule is probably too broad), so this is a quarantine
		// rather than a 429 the worker would retry.
		s.recordEvent(ctx, uid, &msg, store.EventQuarantined, "rate_limited", why, "")
		log.Warn("ingest: per-user cap reached", "detail", why)
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestQuarantined})
		return
	}

	text := strings.TrimSpace(msg.Text)
	out, err := s.ingestLead(ctx, leadInput{
		UserID:      uid,
		Text:        text,
		Now:         now,
		Source:      store.SourceEmail,
		MessageID:   msg.MessageID,
		FromAddress: msg.SenderAddress(),
		Subject:     msg.Subject,
		ReceivedAt:  receivedAt(&msg, now),
		Email: &llm.EmailMeta{
			From:       msg.SenderAddress(),
			FromName:   msg.From.Name,
			Subject:    msg.Subject,
			ReceivedAt: receivedAt(&msg, now),
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, errExtractionFailed):
			// A 5xx is the worker's signal to retry, which is what we want:
			// the model was briefly unavailable, the lead is still good.
			log.Error("ingest: extraction failed", "err", err)
			httpError(w, http.StatusBadGateway, "extraction unavailable")
		default:
			log.Error("ingest: failed", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	if out.Duplicate {
		log.Info("ingest: duplicate", "id", out.Submission.ID)
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestDuplicate, ID: out.Submission.ID})
		return
	}
	log.Info("ingest: lead captured",
		"id", out.Submission.ID,
		"from", msg.SenderAddress(),
		"needs_attention", out.Verdict.NeedsAttention,
	)
	writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestAccepted, ID: out.Submission.ID})
}

// resolveInbound maps an envelope recipient to its owner. It reports failure
// for an address on the wrong domain, an unknown local part, an address whose
// routing rule was never provisioned, and a retired address — the caller
// treats all of them identically so none is distinguishable from outside.
func (s *Server) resolveInbound(r *http.Request, to string) (*store.InboundAddress, int64, bool) {
	local, domain, ok := ingest.SplitAddress(to)
	if !ok || domain != s.cfg.InboundEmailDomain {
		s.log.Warn("ingest: recipient outside the inbound domain", "to", to)
		return nil, 0, false
	}
	addr, err := s.store.LookupInboundAddress(r.Context(), local)
	if err != nil {
		s.log.Error("ingest: address lookup failed", "err", err)
		return nil, 0, false
	}
	if addr == nil {
		s.log.Warn("ingest: unknown capture address")
		return nil, 0, false
	}
	if !addr.Active {
		// Mail to a rotated-away address. It still resolves, so this is
		// logged against the user rather than lost in the dark, but it is not
		// accepted: rotation exists to stop mail reaching the old address.
		s.log.Warn("ingest: mail to a retired address", "user_id", addr.UserID, "address_id", addr.ID)
		return nil, 0, false
	}
	if !addr.Provisioned() {
		s.log.Warn("ingest: mail to an address with no routing rule", "user_id", addr.UserID)
		return nil, 0, false
	}
	return addr, addr.UserID, true
}

// overIngestCap reports whether the user has exhausted their extraction
// budget. Two mechanisms with two jobs: a burst limiter absorbs a delivery
// spike, and a persistent daily count bounds the bill — the latter matters
// because an in-memory limiter resets on restart and this must not.
func (s *Server) overIngestCap(ctx context.Context, uid int64, now time.Time) (bool, string) {
	if !s.ingestLimiter.get("user:" + strconv.FormatInt(uid, 10)).Allow() {
		return true, "burst limit exceeded"
	}
	midnight := now.Truncate(24 * time.Hour)
	n, err := s.store.EmailsToday(ctx, uid, midnight)
	if err != nil {
		// Fail closed. Letting an unbounded number of extractions through
		// because a COUNT failed is the expensive direction to be wrong in.
		s.log.Error("ingest: daily cap check failed", "err", err)
		return true, "could not verify the daily cap"
	}
	if n >= s.cfg.IngestDailyCap {
		return true, "daily cap of " + strconv.Itoa(s.cfg.IngestDailyCap) + " reached"
	}
	return false, ""
}

// recordEvent stores one piece of mail that did not become a lead, so the user
// can see it and rescue it. A failure to record is logged loudly: it is the
// only path by which a lead can actually go missing.
func (s *Server) recordEvent(ctx context.Context, uid int64, msg *ingest.Message,
	status store.EventStatus, reason, detail, text string) int64 {
	ev := &store.IngestionEvent{
		UserID:      uid,
		Status:      status,
		Reason:      reason,
		Detail:      detail,
		MessageID:   msg.MessageID,
		FromAddress: msg.SenderAddress(),
		FromName:    msg.From.Name,
		Subject:     msg.Subject,
		ReceivedAt:  receivedAt(msg, time.Now().UTC()),
		BodyExcerpt: excerpt(firstNonEmpty(text, msg.Text, msg.HTML), nil),
		BodyText:    firstNonEmpty(text, msg.Text),
	}
	if err := s.store.InsertEvent(ctx, ev); err != nil {
		s.log.Error("ingest: could not record event — mail may be lost",
			"user_id", uid, "reason", reason, "err", err)
		return 0
	}
	return ev.ID
}

func receivedAt(msg *ingest.Message, fallback time.Time) time.Time {
	if !msg.ReceivedAt.IsZero() {
		return msg.ReceivedAt
	}
	if !msg.Date.IsZero() {
		return msg.Date
	}
	return fallback
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
