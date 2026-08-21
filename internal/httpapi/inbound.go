package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/EOEboh/ziga-data/internal/ingest"
	"github.com/EOEboh/ziga-data/internal/store"
)

// routeProvisioner creates and releases the mail-routing rule that makes a
// capture address deliverable. An interface so tests can substitute a fake:
// provisioning is a network call to a third party, and every failure path
// through it needs covering.
type routeProvisioner interface {
	CreateAddressRule(ctx context.Context, address string) (string, error)
	DeleteRule(ctx context.Context, ruleID string) error
}

// retiredAddressGrace is how long a rotated-away address keeps routing before
// its rule is released. The user's forwarding rule still points at the old
// address until they update it, and mail is already in flight, so releasing
// the rule immediately would bounce leads that were on their way.
const retiredAddressGrace = 14 * 24 * time.Hour

type inboundResponse struct {
	// Address is the full capture address, or empty when the user has not
	// enabled email capture.
	Address string `json:"address,omitempty"`
	Enabled bool   `json:"enabled"`
	// Domain lets the UI explain the feature before the user opts in.
	Domain    string     `json:"domain"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

func (s *Server) inboundResponseFor(addr *store.InboundAddress) inboundResponse {
	resp := inboundResponse{Domain: s.cfg.InboundEmailDomain}
	// An address with no routing rule cannot receive mail, so it is reported
	// as not enabled: showing it would invite the user to set up forwarding
	// that silently fails.
	if addr != nil && addr.Provisioned() {
		created := addr.CreatedAt
		resp.Address = addr.LocalPart + "@" + s.cfg.InboundEmailDomain
		resp.Enabled = true
		resp.CreatedAt = &created
	}
	return resp
}

// handleInbound returns the user's capture address, if they have one.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	addr, err := s.store.ActiveInboundAddress(r.Context(), userID(r))
	if err != nil {
		s.log.Error("inbound: lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, s.inboundResponseFor(addr))
}

// handleInboundEnable provisions the user's capture address.
//
// Provisioning is an explicit opt-in rather than something that happens at
// signup, for two reasons: it makes a network call to the mail provider, which
// has no business being in the signup path; and each address consumes one of a
// capped number of routing rules, so spending one on every account that never
// uses the feature would exhaust the budget for the users who do.
func (s *Server) handleInboundEnable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(r)

	existing, err := s.store.ActiveInboundAddress(ctx, uid)
	if err != nil {
		s.log.Error("inbound: lookup failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if existing != nil && existing.Provisioned() {
		writeJSON(w, http.StatusOK, s.inboundResponseFor(existing))
		return
	}

	// Reuse a half-provisioned row rather than minting a second address: a
	// retry after a provider outage must not leave the user with two.
	addr := existing
	if addr == nil {
		if !s.inboundBudgetAvailable(ctx, w) {
			return
		}
		local, err := ingest.NewLocalPart()
		if err != nil {
			s.log.Error("inbound: address generation failed", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		addr, err = s.store.CreateInboundAddress(ctx, uid, local)
		if err != nil {
			s.log.Error("inbound: create failed", "err", err)
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	full := addr.LocalPart + "@" + s.cfg.InboundEmailDomain
	ruleID, err := s.cfRoutes.CreateAddressRule(ctx, full)
	if err != nil {
		// The row stays unprovisioned and the next attempt reuses it, so a
		// retry is idempotent and no rule is ever orphaned. The user is told
		// to retry rather than being handed a dead address.
		s.log.Error("inbound: routing rule provisioning failed", "user_id", uid, "err", err)
		httpError(w, http.StatusBadGateway, "Could not finish setting up your address. Try again")
		return
	}
	if err := s.store.SetInboundRuleID(ctx, uid, addr.ID, ruleID); err != nil {
		// The rule exists but we failed to record it. Release it rather than
		// leak a rule nothing will ever reclaim.
		s.log.Error("inbound: could not record rule id, releasing it", "user_id", uid, "err", err)
		if delErr := s.cfRoutes.DeleteRule(ctx, ruleID); delErr != nil {
			s.log.Error("inbound: orphaned routing rule — release it by hand",
				"rule_id", ruleID, "address", full, "err", delErr)
		}
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}

	addr.CFRuleID = ruleID
	s.log.Info("inbound: capture address enabled", "user_id", uid, "address_id", addr.ID)
	writeJSON(w, http.StatusOK, s.inboundResponseFor(addr))
}

// handleInboundRotate issues a new capture address and retires the old one.
//
// The old address keeps routing for a grace period: the user's forwarding rule
// still points at it until they update it, and mail is already in flight.
func (s *Server) handleInboundRotate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(r)

	// A rotating user transiently holds two rules, so the budget is checked
	// before the new one is taken.
	if !s.inboundBudgetAvailable(ctx, w) {
		return
	}

	local, err := ingest.NewLocalPart()
	if err != nil {
		s.log.Error("inbound: address generation failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	full := local + "@" + s.cfg.InboundEmailDomain
	ruleID, err := s.cfRoutes.CreateAddressRule(ctx, full)
	if err != nil {
		// Nothing has changed yet, so the user keeps the address they had.
		s.log.Error("inbound: rotation provisioning failed", "user_id", uid, "err", err)
		httpError(w, http.StatusBadGateway, "Could not issue a new address. Try again")
		return
	}

	// Retire only once the replacement actually routes: the reverse order
	// would leave the user with no working address if provisioning failed.
	if err := s.store.RetireInboundAddresses(ctx, uid); err != nil {
		s.log.Error("inbound: retire failed", "err", err)
		if delErr := s.cfRoutes.DeleteRule(ctx, ruleID); delErr != nil {
			s.log.Error("inbound: orphaned routing rule — release it by hand", "rule_id", ruleID, "err", delErr)
		}
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	addr, err := s.store.CreateInboundAddress(ctx, uid, local)
	if err != nil {
		s.log.Error("inbound: create failed after rotation", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.store.SetInboundRuleID(ctx, uid, addr.ID, ruleID); err != nil {
		s.log.Error("inbound: could not record rotated rule id", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	addr.CFRuleID = ruleID

	s.log.Info("inbound: capture address rotated", "user_id", uid, "address_id", addr.ID)
	writeJSON(w, http.StatusOK, s.inboundResponseFor(addr))
}

// inboundBudgetAvailable reports whether another address can be provisioned,
// writing the error response when it cannot.
//
// The mail provider caps routing rules per domain. Checking our own budget
// first means the user gets an explanation and we get a log line, instead of
// an opaque rejection from the provider at the moment we run out.
func (s *Server) inboundBudgetAvailable(ctx context.Context, w http.ResponseWriter) bool {
	n, err := s.store.CountActiveInboundAddresses(ctx)
	if err != nil {
		s.log.Error("inbound: budget check failed", "err", err)
		httpError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if n >= s.cfg.IngestMaxAddresses {
		s.log.Error("inbound: capture address budget exhausted — raise the routing rule limit",
			"active", n, "budget", s.cfg.IngestMaxAddresses)
		httpError(w, http.StatusServiceUnavailable,
			"Email capture is at capacity right now. We've been notified — try again shortly")
		return false
	}
	return true
}

// ReleaseRetiredAddresses deletes the routing rules of addresses whose grace
// period has elapsed, then removes the rows. Routing rules are a capped
// resource, so an address that is never released is budget permanently lost.
func (s *Server) ReleaseRetiredAddresses(ctx context.Context) {
	s.releaseRetiredBefore(ctx, time.Now().UTC().Add(-retiredAddressGrace))
}

// releaseRetiredBefore takes the cutoff explicitly so the sweep can be tested
// without waiting out the grace period.
func (s *Server) releaseRetiredBefore(ctx context.Context, cutoff time.Time) {
	if s.cfRoutes == nil {
		return
	}
	due, err := s.store.RetiredInboundAddressesBefore(ctx, cutoff)
	if err != nil {
		s.log.Error("inbound: could not list retired addresses", "err", err)
		return
	}
	for _, addr := range due {
		if addr.CFRuleID != "" {
			if err := s.cfRoutes.DeleteRule(ctx, addr.CFRuleID); err != nil {
				// Leave the row in place so the next sweep retries; deleting
				// it would strand the rule with nothing pointing at it.
				s.log.Error("inbound: could not release routing rule",
					"rule_id", addr.CFRuleID, "address_id", addr.ID, "err", err)
				continue
			}
		}
		if err := s.store.DeleteInboundAddress(ctx, addr.ID); err != nil {
			s.log.Error("inbound: could not delete retired address", "address_id", addr.ID, "err", err)
			continue
		}
		s.log.Info("inbound: released retired address", "address_id", addr.ID, "user_id", addr.UserID)
	}
}
