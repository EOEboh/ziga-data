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

	// The filter pipeline runs before anything that costs money. Order within
	// it is cheapest-first; see ingest.Screen.
	opt, err := s.screenOptions(ctx, uid)
	if err != nil {
		log.Error("ingest: could not load filter options", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	outcome := ingest.Screen(msg, opt)

	if outcome.Verification {
		// Not a lead: the code and link a user needs to finish setting up
		// forwarding. Recorded so the setup screen can surface it.
		s.recordVerification(ctx, uid, &msg, outcome)
		log.Info("ingest: forwarding confirmation captured", "has_code", outcome.Code != "")
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestVerification})
		return
	}
	if outcome.Quarantine {
		s.recordEvent(ctx, uid, &msg, store.EventQuarantined, string(outcome.Reason), outcome.Detail, outcome.Text)
		log.Info("ingest: filtered", "reason", outcome.Reason, "detail", outcome.Detail)
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestQuarantined})
		return
	}

	// Dedup runs before the cap so a mail system's retry never spends budget.
	if prior, err := s.priorForMessage(ctx, uid, &msg, now); err != nil {
		log.Error("ingest: dedup lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	} else if prior != nil {
		log.Info("ingest: duplicate", "id", prior.ID)
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestDuplicate, ID: prior.ID})
		return
	}

	if over, why := s.overIngestCap(ctx, uid, now); over {
		// The user, not the worker, is the one who has to act here (their
		// forwarding rule is probably too broad), so this is a quarantine
		// rather than a 429 the worker would retry.
		s.recordEvent(ctx, uid, &msg, store.EventQuarantined, string(ingest.ReasonRateLimited), why, outcome.Text)
		log.Warn("ingest: per-user cap reached", "detail", why)
		writeJSON(w, http.StatusAccepted, ingestResponse{Status: ingestQuarantined})
		return
	}

	// Who is the lead? Almost never the envelope sender once forwarding is
	// involved — see ingest.ResolveOrigin.
	screened := msg
	screened.Text = outcome.Text
	origin := ingest.ResolveOrigin(screened, opt)

	// Truncation happens after forwarded parsing: a long quoted thread often
	// reduces to one short message, and truncating first would cut the lead
	// out of a message that then fits comfortably.
	text, truncated := ingest.Truncate(origin.Text)
	var extraFlags []string
	if truncated {
		// Surfaced through the review pane's existing flag rendering, so the
		// user knows the model did not see the whole message.
		extraFlags = append(extraFlags, "long email truncated for extraction")
	}
	if origin.Provenance.Confidence != "high" {
		// Attributing a lead to the wrong person is a silent failure — the row
		// looks entirely normal. When the choice involved a guess, say so, so
		// the user checks the sender rather than trusting it.
		extraFlags = append(extraFlags, "forwarded email — sender identified with "+
			origin.Provenance.Confidence+" confidence, check who this is from")
	}

	forwardedBy := ""
	if origin.Forwarded && origin.Sender.Address != msg.SenderAddress() {
		forwardedBy = msg.SenderAddress()
	}

	out, err := s.ingestLead(ctx, leadInput{
		UserID:      uid,
		Text:        text,
		Now:         now,
		Source:      store.SourceEmail,
		MessageID:   msg.MessageID,
		FromAddress: strings.ToLower(origin.Sender.Address),
		Subject:     origin.Subject,
		ReceivedAt:  receivedAt(&msg, now),
		ExtraFlags:  extraFlags,
		Email: &llm.EmailMeta{
			From:        strings.ToLower(origin.Sender.Address),
			FromName:    origin.Sender.Name,
			Subject:     origin.Subject,
			ForwardedBy: forwardedBy,
			Forwarded:   origin.Forwarded,
			ReceivedAt:  receivedAt(&msg, now),
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
	// Provenance goes to the log rather than the row: when a user reports that
	// a lead names the wrong person, this says which rule chose them and who
	// else was on the table. Without it the complaint is unfalsifiable.
	prov, _ := json.Marshal(origin.Provenance)
	log.Info("ingest: lead captured",
		"id", out.Submission.ID,
		"envelope_from", msg.SenderAddress(),
		"lead_from", origin.Sender.Address,
		"forwarded", origin.Forwarded,
		"provenance", string(prov),
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

// screenOptions loads the per-user inputs the filter pipeline needs. The
// pipeline itself never touches the store, which is what lets the fixture
// corpus run without one.
func (s *Server) screenOptions(ctx context.Context, uid int64) (ingest.Options, error) {
	blocked, err := s.store.BlockedSenders(ctx, uid)
	if err != nil {
		return ingest.Options{}, err
	}
	opt := ingest.Options{BlockedSenders: blocked}

	// The user's own addresses are how a message they forwarded themselves is
	// recognised — without them, a forwarded thread the user started makes the
	// user their own lead.
	if u, err := s.store.GetUser(ctx, uid); err == nil && u != nil {
		opt.OwnAddresses = append(opt.OwnAddresses, strings.ToLower(u.Email))
	}
	if addr, err := s.store.ActiveInboundAddress(ctx, uid); err == nil && addr != nil {
		opt.OwnAddresses = append(opt.OwnAddresses, addr.LocalPart+"@"+s.cfg.InboundEmailDomain)
	}
	return opt, nil
}

// priorForMessage reports an existing submission for this message, checking
// Message-ID and then the content hash. Both are needed: the hash buckets by
// calendar day so a redelivery across midnight UTC misses it, and plenty of
// mail carries no Message-ID at all.
func (s *Server) priorForMessage(ctx context.Context, uid int64, msg *ingest.Message, now time.Time) (*store.Submission, error) {
	if msg.MessageID != "" {
		prior, err := s.store.FindByMessageID(ctx, uid, msg.MessageID, now.Add(-messageIDDedupWindow))
		if err != nil || prior != nil {
			return prior, err
		}
	}
	return s.store.FindByHash(ctx, uid, store.ContentHash(uid, strings.TrimSpace(msg.Text), nil, now))
}

// recordVerification stores a forwarding-confirmation handshake so the setup
// screen can show the user their code and link.
func (s *Server) recordVerification(ctx context.Context, uid int64, msg *ingest.Message, outcome ingest.Outcome) {
	ev := &store.IngestionEvent{
		UserID:      uid,
		Status:      store.EventVerification,
		Reason:      "forwarding_confirmation",
		Detail:      "confirmation from " + msg.SenderAddress(),
		MessageID:   msg.MessageID,
		FromAddress: msg.SenderAddress(),
		FromName:    msg.From.Name,
		Subject:     msg.Subject,
		ReceivedAt:  receivedAt(msg, time.Now().UTC()),
		BodyExcerpt: excerpt(firstNonEmpty(msg.Text, msg.HTML), nil),
		VerifyCode:  outcome.Code,
		VerifyURL:   outcome.URL,
	}
	if err := s.store.InsertEvent(ctx, ev); err != nil {
		// Without this row the user never sees their code and cannot finish
		// setting up forwarding, so it is worth a loud log.
		s.log.Error("ingest: could not record forwarding confirmation — the user cannot complete setup",
			"user_id", uid, "err", err)
	}
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
