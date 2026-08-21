package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EOEboh/ziga-data/internal/ingest"
	"github.com/EOEboh/ziga-data/internal/store"
)

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func listQuarantine(t *testing.T, h http.Handler, query string) quarantineResponse {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/api/quarantine"+query, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("quarantine list: %d %s", rec.Code, rec.Body)
	}
	var out quarantineResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// newsletterTo builds a message the machine filter will quarantine.
func newsletterTo(to string) ingest.Message {
	m := mailTo(to)
	m.MessageID = "<news-1@campaign.example.com>"
	m.From = ingest.Identity{Name: "Toolkit News", Address: "news@toolkit.example.com"}
	m.Subject = "New in Toolkit: automations"
	m.Headers = map[string][]string{"list-unsubscribe": {"<https://toolkit.example.com/unsub>"}}
	m.Text = "Automations are here. Build workflows without code, and ship faster than ever before."
	return m
}

func TestFilteredMailIsVisibleNotLost(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	rec, out := postIngest(t, rawHandler(s), newsletterTo(addr))
	if rec.Code != http.StatusAccepted || out.Status != ingestQuarantined {
		t.Fatalf("a newsletter must be filtered before extraction: %d %+v", rec.Code, out)
	}
	// The whole point of the filter is that it runs before the model.
	if calls := s.extractor.(*fakeExtractor).calls; calls != 0 {
		t.Fatalf("the extractor ran %d times on filtered mail — the filter is a cost control and must run first", calls)
	}

	items := listQuarantine(t, h, "")
	if len(items.Items) != 1 {
		t.Fatalf("want the filtered message visible to the user, got %d items", len(items.Items))
	}
	item := items.Items[0]
	if item.Reason != string(ingest.ReasonMachineMail) {
		t.Errorf("reason = %q", item.Reason)
	}
	// The user has to be able to see what it was and decide.
	if item.FromAddress != "news@toolkit.example.com" || item.Subject == "" || item.Excerpt == "" {
		t.Errorf("a quarantined item must be identifiable: %+v", item)
	}
	// And why it was filtered, without reading server logs.
	if !strings.Contains(item.Detail, "list-unsubscribe") {
		t.Errorf("detail = %q, want the rule that fired", item.Detail)
	}
	if !item.Rescuable {
		t.Error("a freshly quarantined item must be rescuable")
	}
}

func TestRescueMovesMailIntoTheReviewQueue(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	postIngest(t, rawHandler(s), newsletterTo(addr))
	item := listQuarantine(t, h, "").Items[0]

	rec := doJSON(t, h, http.MethodPost, "/api/quarantine/"+strconv.FormatInt(item.ID, 10)+"/rescue", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rescue: %d %s", rec.Code, rec.Body)
	}
	var sub submissionResponse
	json.Unmarshal(rec.Body.Bytes(), &sub)
	if sub.ID == 0 {
		t.Fatal("rescue must produce a reviewable submission")
	}

	// A rescue is the user overriding the filter, so it must actually extract.
	if calls := s.extractor.(*fakeExtractor).calls; calls != 1 {
		t.Errorf("extractor calls = %d, want 1", calls)
	}
	stored, err := s.store.Get(context.Background(), uid, sub.ID)
	if err != nil || stored == nil {
		t.Fatal(err)
	}
	if stored.Status != store.StatusPending {
		t.Errorf("a rescued lead still waits for review, got status %q", stored.Status)
	}

	// It leaves the quarantine list, and cannot be rescued twice into two leads.
	if got := listQuarantine(t, h, ""); len(got.Items) != 0 {
		t.Errorf("rescued item still listed as quarantined: %+v", got.Items)
	}
	again := doJSON(t, h, http.MethodPost, "/api/quarantine/"+strconv.FormatInt(item.ID, 10)+"/rescue", nil)
	if again.Code == http.StatusOK {
		var second submissionResponse
		json.Unmarshal(again.Body.Bytes(), &second)
		if second.ID != sub.ID {
			t.Errorf("rescuing twice created a second lead (%d then %d)", sub.ID, second.ID)
		}
	}
}

func TestDismissClosesWithoutExtracting(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	addr := enableInbound(t, s, testUID(t, s))

	postIngest(t, rawHandler(s), newsletterTo(addr))
	item := listQuarantine(t, h, "").Items[0]

	rec := doJSON(t, h, http.MethodPost, "/api/quarantine/"+strconv.FormatInt(item.ID, 10)+"/dismiss", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if calls := s.extractor.(*fakeExtractor).calls; calls != 0 {
		t.Errorf("dismiss must not extract, got %d calls", calls)
	}
	if got := listQuarantine(t, h, ""); len(got.Items) != 0 {
		t.Errorf("dismissed item still listed: %+v", got.Items)
	}
}

func TestPurgedMailCannotBeRescued(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)
	enableInbound(t, s, uid)

	// An item whose body retention has cleared. The UI must not offer an
	// action that cannot work, and the endpoint must say why plainly.
	ev := &store.IngestionEvent{
		UserID: uid, Status: store.EventQuarantined, Reason: "machine_mail",
		FromAddress: "news@toolkit.example.com", Subject: "old", BodyExcerpt: "an old newsletter",
	}
	if err := s.store.InsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	items := listQuarantine(t, h, "")
	if len(items.Items) != 1 || items.Items[0].Rescuable {
		t.Fatalf("an item with no body must report itself unrescuable: %+v", items.Items)
	}
	rec := doJSON(t, h, http.MethodPost, "/api/quarantine/"+strconv.FormatInt(ev.ID, 10)+"/rescue", nil)
	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410 with an explanation", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too old") {
		t.Errorf("the user must be told why, got: %s", rec.Body)
	}
}

func TestVerificationCodeIsSurfacedNotExtracted(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	addr := enableInbound(t, s, testUID(t, s))

	confirm := mailTo(addr)
	confirm.MessageID = "<forwarding-33821484@google.com>"
	confirm.From = ingest.Identity{Name: "Gmail Team", Address: "forwarding-noreply@google.com"}
	confirm.EnvelopeFrom = "forwarding-noreply@google.com"
	confirm.Subject = "Gmail Forwarding Confirmation (#338214849) - Receive Mail from owner@gmail.com"
	confirm.Text = "Confirmation code: 338214849\n\nTo allow this, click:\n" +
		"https://mail.google.com/mail/vf-%5BANGjdJ8x2%5D-DhSl3nQ4kZ\n"

	rec, out := postIngest(t, rawHandler(s), confirm)
	if rec.Code != http.StatusAccepted || out.Status != ingestVerification {
		t.Fatalf("the forwarding confirmation must be recognised: %d %+v", rec.Code, out)
	}
	// It is not a lead, and must not be billed as one.
	if calls := s.extractor.(*fakeExtractor).calls; calls != 0 {
		t.Errorf("the confirmation was sent to the model (%d calls); it is a setup step, not a lead", calls)
	}
	// It must NOT be in the ordinary quarantine list, where the user has no
	// reason to look before their forwarding works.
	if got := listQuarantine(t, h, ""); len(got.Items) != 0 {
		t.Errorf("the confirmation is sitting in general quarantine: %+v", got.Items)
	}

	pending := listQuarantine(t, h, "?status=verification")
	if len(pending.Items) != 1 {
		t.Fatalf("want the confirmation surfaced for setup, got %d", len(pending.Items))
	}
	item := pending.Items[0]
	if item.VerifyCode != "338214849" {
		t.Errorf("code = %q — without it the user cannot finish setup", item.VerifyCode)
	}
	if !strings.HasPrefix(item.VerifyURL, "https://mail.google.com/mail/") {
		t.Errorf("url = %q", item.VerifyURL)
	}
}

func TestBlockSenderRefusesToBreakForwardingSetup(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)

	for _, pattern := range []string{"forwarding-noreply@google.com", "@google.com"} {
		rec := doJSON(t, h, http.MethodPost, "/api/senders/block", map[string]string{"pattern": pattern})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("blocking %q returned %d; it would silently stop the user ever receiving another confirmation code",
				pattern, rec.Code)
		}
	}

	// Ordinary senders remain blockable, and blocking then works.
	rec := doJSON(t, h, http.MethodPost, "/api/senders/block", map[string]string{"pattern": "Sales@Spammy.Example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body)
	}
	var listed struct {
		Patterns []string `json:"patterns"`
	}
	json.Unmarshal(doJSON(t, h, http.MethodGet, "/api/senders/blocked", nil).Body.Bytes(), &listed)
	if len(listed.Patterns) != 1 || listed.Patterns[0] != "sales@spammy.example.com" {
		t.Fatalf("blocklist = %v, want the pattern normalised", listed.Patterns)
	}
}

func TestBlockedSenderIsFilteredOnArrival(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	addr := enableInbound(t, s, testUID(t, s))

	if rec := doJSON(t, h, http.MethodPost, "/api/senders/block",
		map[string]string{"pattern": "@spammy.example.com"}); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body)
	}

	spam := mailTo(addr)
	spam.From = ingest.Identity{Name: "Cold Outreach", Address: "sales@spammy.example.com"}
	spam.EnvelopeFrom = "sales@spammy.example.com"
	spam.MessageID = "<spam-1@spammy.example.com>"
	spam.Text = "Hello, I noticed your website could rank higher on Google. Interested in a call?"

	rec, out := postIngest(t, rawHandler(s), spam)
	if out.Status != ingestQuarantined {
		t.Fatalf("blocked sender must be filtered: %d %+v", rec.Code, out)
	}
	if calls := s.extractor.(*fakeExtractor).calls; calls != 0 {
		t.Errorf("blocked mail reached the model (%d calls)", calls)
	}

	item := listQuarantine(t, h, "").Items[0]
	if item.Reason != string(ingest.ReasonBlockedSender) {
		t.Errorf("reason = %q, want %q", item.Reason, ingest.ReasonBlockedSender)
	}
}

func TestBlockSenderRejectsNonsense(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	for _, pattern := range []string{"", "   ", "not-an-address", "@", "@nodot", "user@nodot"} {
		if rec := doJSON(t, h, http.MethodPost, "/api/senders/block",
			map[string]string{"pattern": pattern}); rec.Code != http.StatusBadRequest {
			t.Errorf("pattern %q returned %d, want 400 — a blocklist entry that matches nothing is worse than none", pattern, rec.Code)
		}
	}
}

func TestQueueSeenTracksCapturedWhileAway(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	if rec := doJSON(t, h, http.MethodPost, "/api/queue/seen", nil); rec.Code != http.StatusOK {
		t.Fatalf("queue seen: %d %s", rec.Code, rec.Body)
	}
	seen, err := s.store.QueueSeenAt(ctx, uid)
	if err != nil || seen.IsZero() {
		t.Fatalf("queue_seen_at not recorded: %v %v", seen, err)
	}

	// Wait past the second boundary the timestamp is stored at, so "after"
	// really is after.
	time.Sleep(1100 * time.Millisecond)
	if _, out := postIngest(t, rawHandler(s), mailTo(addr)); out.Status != ingestAccepted {
		t.Fatalf("ingest: %+v", out)
	}

	n, err := s.store.CountCapturedSince(ctx, uid, seen)
	if err != nil || n != 1 {
		t.Fatalf("want 1 lead captured since the queue was last seen, got %d err=%v", n, err)
	}
}
