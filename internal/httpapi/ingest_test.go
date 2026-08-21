package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/EOEboh/ziga-data/internal/ingest"
	"github.com/EOEboh/ziga-data/internal/store"
)

const (
	testIngestSecret = "test-ingest-secret-at-least-32-chars"
	testInboundDoma  = "in.test.example"
)

// fakeRoutes stands in for the mail provider's routing-rule API.
type fakeRoutes struct {
	created  map[string]string // address -> rule id
	deleted  []string
	next     int
	failNext error
}

func newFakeRoutes() *fakeRoutes { return &fakeRoutes{created: map[string]string{}} }

func (f *fakeRoutes) CreateAddressRule(_ context.Context, address string) (string, error) {
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return "", err
	}
	f.next++
	id := fmt.Sprintf("rule-%d", f.next)
	f.created[strings.ToLower(address)] = id
	return id, nil
}

func (f *fakeRoutes) DeleteRule(_ context.Context, ruleID string) error {
	f.deleted = append(f.deleted, ruleID)
	for addr, id := range f.created {
		if id == ruleID {
			delete(f.created, addr)
		}
	}
	return nil
}

// ingestServer builds a server with email ingestion configured and a fake
// routing provisioner.
func ingestServer(t *testing.T) (*Server, *fakeRoutes) {
	t.Helper()
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	s.cfg.InboundEmailDomain = testInboundDoma
	s.cfg.IngestSharedSecret = testIngestSecret
	s.cfg.CloudflareAPIToken = "token"
	s.cfg.CloudflareZoneID = "zone"
	s.cfg.IngestWorkerName = "worker"
	s.cfg.IngestDailyCap = 50
	s.cfg.IngestBurst = 10
	s.cfg.IngestMaxAddresses = 180
	// Rebuild the limiter now that the burst is configured.
	s.ingestLimiter = newIPLimiterBurst(s.cfg.IngestBurst*6, s.cfg.IngestBurst)
	routes := newFakeRoutes()
	s.cfRoutes = routes
	return s, routes
}

// enableInbound provisions a capture address for a user directly through the
// store, returning the full address.
func enableInbound(t *testing.T, s *Server, uid int64) string {
	t.Helper()
	ctx := context.Background()
	local, err := ingest.NewLocalPart()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.store.CreateInboundAddress(ctx, uid, local)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetInboundRuleID(ctx, uid, addr.ID, "rule-x"); err != nil {
		t.Fatal(err)
	}
	return local + "@" + testInboundDoma
}

func mailTo(to string) ingest.Message {
	return ingest.Message{
		Version:      ingest.PayloadVersion,
		To:           to,
		EnvelopeFrom: "ada@lumen.studio",
		MessageID:    "<m-" + to + "@mail>",
		From:         ingest.Identity{Name: "Ada Okafor", Address: "ada@lumen.studio"},
		Subject:      "landing page for the launch",
		Date:         time.Now().UTC(),
		ReceivedAt:   time.Now().UTC(),
		Headers:      map[string][]string{},
		Text:         "Hi, I need a landing page for our launch in March. Reach me on ada@lumen.studio.",
	}
}

// postIngest signs and posts a payload the way the mail worker would.
func postIngest(t *testing.T, h http.Handler, msg ingest.Message) (*httptest.ResponseRecorder, ingestResponse) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return postIngestRaw(t, h, body, ts, ingest.Sign([]byte(testIngestSecret), ts, body))
}

func postIngestRaw(t *testing.T, h http.Handler, body []byte, ts, sig string) (*httptest.ResponseRecorder, ingestResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/email", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ts != "" {
		req.Header.Set(ingest.TimestampHeader, ts)
	}
	if sig != "" {
		req.Header.Set(ingest.SignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out ingestResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// rawHandler builds the route tree WITHOUT the session/CSRF cookie injection
// that handler() does. The ingestion endpoint must work with no cookies at
// all; injecting them would hide a failure to exclude it from that middleware.
func rawHandler(s *Server) http.Handler {
	return s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}})
}

func TestIngestAcceptsASignedLead(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s) // seeds the user
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)
	_ = h

	rec, out := postIngest(t, rawHandler(s), mailTo(addr))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if out.Status != ingestAccepted || out.ID == 0 {
		t.Fatalf("want an accepted lead with an id, got %+v", out)
	}

	sub, err := s.store.Get(context.Background(), uid, out.ID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}
	if sub.Source != store.SourceEmail {
		t.Errorf("source = %q, want email", sub.Source)
	}
	if sub.FromAddress != "ada@lumen.studio" {
		t.Errorf("from = %q, want the sender recorded so the user can see where it came from", sub.FromAddress)
	}
	if sub.Status != store.StatusPending {
		t.Errorf("status = %q — an ingested lead must wait for review like any other", sub.Status)
	}
}

func TestIngestRequiresNoCSRFCookie(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	addr := enableInbound(t, s, testUID(t, s))

	rec, _ := postIngest(t, rawHandler(s), mailTo(addr))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the worker has no cookie jar; ingestion must not require CSRF. status=%d body=%s", rec.Code, rec.Body)
	}
	// It must also not try to establish browser state on a machine caller.
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookie || c.Name == sessionCookie {
			t.Errorf("ingestion response set a %s cookie; it is not a browser endpoint", c.Name)
		}
	}
}

func TestIngestRejectsBadSignatures(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	addr := enableInbound(t, s, testUID(t, s))
	h := rawHandler(s)

	body, _ := json.Marshal(mailTo(addr))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	good := ingest.Sign([]byte(testIngestSecret), ts, body)

	tampered := append([]byte{}, body...)
	tampered = bytes.Replace(tampered, []byte("ada@lumen.studio"), []byte("evil@attacker.io"), 1)

	staleTS := strconv.FormatInt(time.Now().Add(-ingest.MaxSkew-time.Minute).Unix(), 10)

	cases := []struct {
		name    string
		body    []byte
		ts, sig string
	}{
		{"no headers at all", body, "", ""},
		{"missing signature", body, ts, ""},
		{"missing timestamp", body, "", good},
		{"garbage signature", body, ts, "v1=not-a-signature"},
		{"signature for a different body", tampered, ts, good},
		{"replayed with a stale timestamp", body, staleTS, ingest.Sign([]byte(testIngestSecret), staleTS, body)},
		{"signed with the wrong secret", body, ts, ingest.Sign([]byte("some-other-secret-thats-long-enough"), ts, body)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := postIngestRaw(t, h, c.body, c.ts, c.sig)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401. body = %s", rec.Code, rec.Body)
			}
			// The response must not explain which check failed.
			if b := strings.ToLower(rec.Body.String()); strings.Contains(b, "timestamp") ||
				strings.Contains(b, "stale") || strings.Contains(b, "mismatch") {
				t.Errorf("rejection body leaks the reason: %s", rec.Body)
			}
		})
	}

	// No lead was created by any of those.
	pending, err := s.store.ListByStatus(context.Background(), testUID(t, s), store.StatusPending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("unauthenticated requests must not create leads, got %d", len(pending))
	}
}

func TestIngestUnknownAddressIsIndistinguishable(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	known := enableInbound(t, s, uid)

	// Retire an address so it resolves but is inactive.
	retiredLocal, _ := ingest.NewLocalPart()
	ctx := context.Background()
	ra, _ := s.store.CreateInboundAddress(ctx, uid, retiredLocal)
	s.store.SetInboundRuleID(ctx, uid, ra.ID, "rule-old")
	s.store.RetireInboundAddresses(ctx, uid)
	// Re-provision the known address, since retiring cleared it too.
	known = enableInbound(t, s, uid)

	h := rawHandler(s)
	unknown := "lead-doesnotexistatall@" + testInboundDoma
	wrongDomain := "lead-abc@somewhere-else.example"

	var bodies []string
	for _, to := range []string{unknown, wrongDomain, retiredLocal + "@" + testInboundDoma} {
		rec, out := postIngest(t, h, mailTo(to))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202 so probing learns nothing from the code", to, rec.Code)
		}
		if out.Status != ingestDiscarded {
			t.Errorf("%s: status = %q, want %q", to, out.Status, ingestDiscarded)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("responses differ between rejection kinds, which lets an attacker\ntell an unknown address from a retired one:\n %s\n %s", bodies[0], bodies[i])
		}
	}

	// And no event rows. The retired address resolves to this user, so a
	// wrongly-recorded event would land under their id.
	count, err := s.store.CountEvents(ctx, uid,
		store.EventQuarantined, store.EventVerification, store.EventRescued, store.EventDismissed)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("mail to an unresolvable address must not create event rows, got %d", count)
	}

	// The known address still works, so the test is not passing vacuously.
	rec, out := postIngest(t, h, mailTo(known))
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("the real address must still accept mail: %d %+v", rec.Code, out)
	}
}

func TestIngestRouteAbsentWhenUnconfigured(t *testing.T) {
	// A server without ingestion configured must not expose the endpoint at
	// all: with no shared secret there is nothing to authenticate against.
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	rec, _ := postIngestRaw(t, rawHandler(s), []byte(`{}`), "1", "v1=x")
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusAccepted {
		t.Fatalf("unconfigured server answered the ingestion route with %d; it should 404", rec.Code)
	}
}

func TestIngestDoesNotTrustEmailHeadersForRateLimiting(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	addr := enableInbound(t, s, testUID(t, s))
	h := rawHandler(s)

	// X-Forwarded-For is a real EMAIL header (Gmail auto-forward sets it) and
	// clientIP trusts it. If ingestion ever keyed a limiter by IP, a sender
	// could choose their own bucket. This asserts the HTTP header is inert
	// here: it must not change the outcome.
	msg := mailTo(addr)
	body, _ := json.Marshal(msg)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ingest.Sign([]byte(testIngestSecret), ts, body)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/email", bytes.NewReader(body))
	req.Header.Set(ingest.TimestampHeader, ts)
	req.Header.Set(ingest.SignatureHeader, sig)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestIngestPerUserDailyCapQuarantinesRatherThanDropping(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)
	s.cfg.IngestDailyCap = 2
	h := rawHandler(s)

	for i := range 2 {
		msg := mailTo(addr)
		msg.MessageID = fmt.Sprintf("<cap-%d@mail>", i)
		msg.Text = fmt.Sprintf("Lead number %d needs a website built for their new bakery.", i)
		rec, out := postIngest(t, h, msg)
		if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
			t.Fatalf("message %d: %d %+v", i, rec.Code, out)
		}
	}

	over := mailTo(addr)
	over.MessageID = "<cap-over@mail>"
	over.Text = "A third lead that arrives after the cap is reached, needing a logo."
	rec, out := postIngest(t, h, over)

	// A 429 would make the worker retry forever; the user is the one who has
	// to act (their forwarding filter is too broad), so it quarantines.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the worker must not retry a per-user cap", rec.Code)
	}
	if out.Status != ingestQuarantined {
		t.Fatalf("status = %q, want %q", out.Status, ingestQuarantined)
	}

	// The lead is visible, not lost.
	events, err := s.store.ListEvents(context.Background(), uid, []store.EventStatus{store.EventQuarantined}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want the over-cap mail recorded so the user can rescue it, got %d events", len(events))
	}
	if events[0].Reason != "rate_limited" {
		t.Errorf("reason = %q, want rate_limited", events[0].Reason)
	}
	if events[0].BodyExcerpt == "" {
		t.Error("a quarantined item with no excerpt is not reviewable")
	}
}

func TestIngestDedupsRedelivery(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)
	h := rawHandler(s)

	msg := mailTo(addr)
	rec1, out1 := postIngest(t, h, msg)
	if rec1.Code != http.StatusAccepted || out1.Status != ingestAccepted {
		t.Fatalf("first delivery: %d %+v", rec1.Code, out1)
	}

	// Mail systems retry. The second delivery must resolve to the same lead
	// rather than creating a second one.
	rec2, out2 := postIngest(t, h, msg)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("second delivery: %d", rec2.Code)
	}
	if out2.Status != ingestDuplicate {
		t.Errorf("status = %q, want %q", out2.Status, ingestDuplicate)
	}
	if out2.ID != out1.ID {
		t.Errorf("redelivery resolved to id %d, want the original %d", out2.ID, out1.ID)
	}

	pending, err := s.store.ListByStatus(context.Background(), uid, store.StatusPending, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("redelivery created %d leads, want 1", len(pending))
	}
}

func TestIngestQueuesLeadEvenWithNoDestination(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	// The seeded user has no destination connected. Capture must still work:
	// losing a lead because the user has not finished setup would be the worst
	// possible outcome of automating capture.
	if d, err := s.store.GetDestination(context.Background(), uid); err == nil && d != nil {
		t.Fatalf("precondition: expected no destination connected, got %v", d)
	}

	rec, out := postIngest(t, rawHandler(s), mailTo(addr))
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("a lead must be queued with no destination connected: %d %+v", rec.Code, out)
	}
}

func TestIngestExtractionFailureAsksForRetry(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	addr := enableInbound(t, s, testUID(t, s))
	s.extractor = &fakeExtractor{err: errors.New("model unavailable")}

	rec, _ := postIngest(t, rawHandler(s), mailTo(addr))
	// A 5xx is the worker's signal to retry. Anything in the 2xx range would
	// tell it to give up on a lead that is still perfectly good.
	if rec.Code < 500 {
		t.Fatalf("status = %d, want 5xx so the worker retries a transient model failure", rec.Code)
	}
}

// TestIngestAttributesForwardedLeadToTheOriginalSender is the end-to-end
// version of the auto-forward rule.
//
// Gmail auto-forwarding preserves the original From: and adds X-Forwarded-*
// headers containing the USER's own addresses. If the pipeline read the
// identity out of those headers instead of trusting From:, every
// auto-forwarded lead would be filed under the user's own mailbox — and the
// rows would look completely normal, so nobody would notice.
func TestIngestAttributesForwardedLeadToTheOriginalSender(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	msg := mailTo(addr)
	msg.From = ingest.Identity{Name: "Chiamaka Eze", Address: "chiamaka@eze-events.com"}
	msg.EnvelopeFrom = "chiamaka@eze-events.com"
	msg.MessageID = "<fwd-1@mail.gmail.com>"
	msg.Subject = "Event branding"
	msg.Headers = map[string][]string{
		"x-forwarded-for": {testUserEmail + " " + addr},
		"x-forwarded-to":  {addr},
	}
	msg.Text = "I'm organising a conference in October for 400 people and need full event branding."

	rec, out := postIngest(t, rawHandler(s), msg)
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("ingest: %d %+v", rec.Code, out)
	}
	sub, err := s.store.Get(context.Background(), uid, out.ID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}
	if sub.FromAddress != "chiamaka@eze-events.com" {
		t.Fatalf("lead recorded as %q, want the original sender", sub.FromAddress)
	}
	if sub.FromAddress == testUserEmail {
		t.Fatal("the lead was attributed to the account owner — the auto-forward rule is inverted")
	}
}

// TestIngestManualForwardReadsTheInnerSender: a user pressing Forward makes
// themselves the envelope sender, and the real lead is inside the body.
func TestIngestManualForwardReadsTheInnerSender(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	msg := mailTo(addr)
	msg.From = ingest.Identity{Name: "Sam Owner", Address: testUserEmail}
	msg.EnvelopeFrom = testUserEmail
	msg.MessageID = "<manual-1@mail.gmail.com>"
	msg.Subject = "Fwd: Need a logo for my agency"
	msg.Headers = map[string][]string{}
	msg.Text = "Passing this on.\n\n---------- Forwarded message ---------\n" +
		"From: Ngozi Umeh <ngozi@umeh-legal.com>\n" +
		"Date: Tue, 18 Aug 2026 at 14:02\n" +
		"Subject: Need a logo for my agency\n\n" +
		"I'm starting a legal consultancy and need a logo plus letterhead."

	rec, out := postIngest(t, rawHandler(s), msg)
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("ingest: %d %+v", rec.Code, out)
	}
	sub, err := s.store.Get(context.Background(), uid, out.ID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}
	if sub.FromAddress != "ngozi@umeh-legal.com" {
		t.Fatalf("lead recorded as %q, want the person inside the forward", sub.FromAddress)
	}
	// The subject should read as the original enquiry, not "Fwd: ...".
	if strings.HasPrefix(strings.ToLower(sub.Subject), "fwd:") {
		t.Errorf("subject = %q, want the forward prefix stripped", sub.Subject)
	}
	// The model must be told this was forwarded and by whom, or it has no way
	// to know the lead is the person inside rather than the sender.
	meta := s.extractor.(*fakeExtractor).last.Email
	if meta == nil {
		t.Fatal("no email metadata reached the extractor")
	}
	if !meta.Forwarded {
		t.Error("the extraction prompt was not told the message was forwarded")
	}
	if meta.From != "ngozi@umeh-legal.com" {
		t.Errorf("prompt metadata names %q as the lead, want the forwarded sender", meta.From)
	}
	if meta.ForwardedBy != testUserEmail {
		t.Errorf("forwarded_by = %q, want the account owner recorded as the forwarder", meta.ForwardedBy)
	}
	// The forwarder's own preamble must not be what the model reads.
	if strings.Contains(s.extractor.(*fakeExtractor).last.Text, "Passing this on") {
		t.Error("the forwarder's preamble was sent to the model as the lead body")
	}
}

// TestLowConfidenceAttributionRaisesAFlag: attributing a lead to the wrong
// person is a silent failure. When the choice involved a guess, the review
// pane must say so rather than presenting it as settled.
func TestLowConfidenceAttributionRaisesAFlag(t *testing.T) {
	s, _ := ingestServer(t)
	_ = handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	// A localised forward we do not parse, sent by the account owner.
	msg := mailTo(addr)
	msg.From = ingest.Identity{Name: "Sam Owner", Address: testUserEmail}
	msg.EnvelopeFrom = testUserEmail
	msg.MessageID = "<localised-1@mail.example.de>"
	msg.Subject = "WG: Anfrage"
	msg.Headers = map[string][]string{}
	msg.Text = "-----Ursprüngliche Nachricht-----\n" +
		"Von: Klaus Weber <klaus@weber-bau.de>\n" +
		"Gesendet: Montag, 17. August 2026 14:14\n\n" +
		"Wir brauchen eine neue Website für unser Bauunternehmen."

	_, out := postIngest(t, rawHandler(s), msg)
	if out.Status != ingestAccepted {
		t.Fatalf("an unparsed forward must still be captured, got %+v", out)
	}
	sub, err := s.store.Get(context.Background(), uid, out.ID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}
	var flags []string
	json.Unmarshal(sub.Flags, &flags)
	found := false
	for _, f := range flags {
		if strings.Contains(f, "confidence") {
			found = true
		}
	}
	if !found {
		t.Errorf("flags = %v, want one warning the user to check who the lead is from", flags)
	}
}
