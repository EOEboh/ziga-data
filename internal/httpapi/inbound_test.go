package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func getInbound(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, inboundResponse) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out inboundResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestInboundStartsDisabled(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)

	rec, out := getInbound(t, h, http.MethodGet, "/api/inbound")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if out.Enabled || out.Address != "" {
		t.Fatalf("email capture must be opt-in, got %+v", out)
	}
	// The domain is still reported so the UI can explain the feature before
	// the user commits a routing rule to it.
	if out.Domain != testInboundDoma {
		t.Errorf("domain = %q, want %q", out.Domain, testInboundDoma)
	}
}

func TestInboundEnableProvisionsARoutingRule(t *testing.T) {
	s, routes := ingestServer(t)
	h := handler(s)

	rec, out := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if !out.Enabled || !strings.HasSuffix(out.Address, "@"+testInboundDoma) {
		t.Fatalf("want an enabled address on the inbound domain, got %+v", out)
	}
	// Without a routing rule the address is undeliverable, so enabling must
	// have created one for exactly this address.
	if _, ok := routes.created[strings.ToLower(out.Address)]; !ok {
		t.Fatalf("no routing rule created for %s; the address would silently receive nothing", out.Address)
	}

	// Enabling twice must not mint a second address or burn a second rule.
	rec2, out2 := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec2.Code != http.StatusOK || out2.Address != out.Address {
		t.Fatalf("re-enabling changed the address: %q -> %q", out.Address, out2.Address)
	}
	if len(routes.created) != 1 {
		t.Errorf("re-enabling consumed %d routing rules, want 1 — they are a capped resource", len(routes.created))
	}
}

func TestInboundEnableFailureLeavesNoDeadAddress(t *testing.T) {
	s, routes := ingestServer(t)
	h := handler(s)
	routes.failNext = errors.New("cloudflare is down")

	rec, _ := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error: handing over an address that cannot receive mail is worse than failing", rec.Code)
	}

	// The user must not be shown an address that routes nowhere.
	_, out := getInbound(t, h, http.MethodGet, "/api/inbound")
	if out.Enabled || out.Address != "" {
		t.Fatalf("a half-provisioned address must not read as enabled, got %+v", out)
	}

	// Retrying reuses the reserved local part rather than minting a second.
	rec, retry := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec.Code != http.StatusOK || !retry.Enabled {
		t.Fatalf("retry after a provider outage must succeed: %d %+v", rec.Code, retry)
	}
	addrs, err := s.store.ActiveInboundAddress(context.Background(), testUID(t, s))
	if err != nil || addrs == nil {
		t.Fatal(err)
	}
	if got := addrs.LocalPart + "@" + testInboundDoma; got != retry.Address {
		t.Errorf("retry produced a different address than it reported: %q vs %q", got, retry.Address)
	}
	if len(routes.created) != 1 {
		t.Errorf("want exactly 1 rule after a failed then successful enable, got %d", len(routes.created))
	}
}

func TestInboundRotateKeepsTheOldAddressRoutingDuringGrace(t *testing.T) {
	s, routes := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)

	_, first := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	rec, second := getInbound(t, h, http.MethodPost, "/api/inbound/rotate")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if second.Address == first.Address {
		t.Fatal("rotation must issue a different address")
	}

	// The user's forwarding rule still points at the old address and mail is
	// already in flight, so its routing rule must survive the rotation.
	if len(routes.deleted) != 0 {
		t.Errorf("rotation released the old rule immediately, bouncing in-flight leads: %v", routes.deleted)
	}
	old, err := s.store.LookupInboundAddress(ctx, strings.Split(first.Address, "@")[0])
	if err != nil || old == nil {
		t.Fatalf("the old address must still resolve: %v %v", old, err)
	}
	if old.Active {
		t.Error("the old address must no longer be active")
	}

	// The scheduled sweep must not touch it while the grace period is running.
	s.ReleaseRetiredAddresses(ctx)
	if len(routes.deleted) != 0 {
		t.Fatalf("sweep released the rule during the grace period: %v", routes.deleted)
	}

	// Once the grace period elapses the rule is released — rules are capped,
	// so one never reclaimed is capacity permanently lost.
	s.releaseRetiredBefore(ctx, time.Now().UTC().Add(time.Hour))
	if len(routes.deleted) != 1 {
		t.Fatalf("want the old rule released after the grace period, deleted = %v", routes.deleted)
	}
	if gone, err := s.store.LookupInboundAddress(ctx, strings.Split(first.Address, "@")[0]); err != nil || gone != nil {
		t.Errorf("the released address row must be removed: %v %v", gone, err)
	}
	// The current address is untouched.
	if cur, err := s.store.ActiveInboundAddress(ctx, uid); err != nil || cur == nil {
		t.Fatalf("the active address must survive the sweep: %v %v", cur, err)
	}
}

func TestInboundBudgetRefusesBeforeTheProviderDoes(t *testing.T) {
	s, routes := ingestServer(t)
	h := handler(s)
	// The provider caps routing rules per domain. We must refuse first, with
	// an explanation, rather than let their opaque rejection reach the user.
	s.cfg.IngestMaxAddresses = 0

	rec, _ := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the address budget is exhausted", rec.Code)
	}
	if len(routes.created) != 0 {
		t.Errorf("no provider call should be made once the budget is known to be full, got %d", len(routes.created))
	}
}

func TestInboundRoutesAbsentWhenUnconfigured(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	h := handler(s)

	// Offering an address the server cannot provision is the failure the
	// Notion all-or-nothing guard exists to prevent; ingestion follows suit.
	// The enable route is simply not mounted.
	rec, _ := getInbound(t, h, http.MethodPost, "/api/inbound/enable")
	if rec.Code == http.StatusOK {
		t.Fatalf("an unconfigured server provisioned an address it cannot route mail to")
	}

	// GET falls through to the SPA catch-all rather than 404ing (that is how
	// every unknown GET behaves, so deep links survive a refresh). What must
	// not happen is it answering with an address payload.
	rec, out := getInbound(t, h, http.MethodGet, "/api/inbound")
	if out.Enabled || out.Address != "" {
		t.Fatalf("unconfigured server reported a capture address: %+v", out)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "application/json") {
		t.Errorf("the inbound route answered as JSON while unconfigured (content-type %q)", ct)
	}

	// And the ingestion webhook itself is unreachable, which is the property
	// that actually matters: no secret means nothing to authenticate against.
	if s.cfg.EmailIngestConfigured() {
		t.Fatal("precondition: this server must not be ingest-configured")
	}
}
