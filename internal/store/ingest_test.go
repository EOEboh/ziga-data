package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// mustUser inserts a user row so the FK-ish relationships in these tests are
// realistic (queue_seen_at lives on users, so it needs a real row).
func mustUser(t *testing.T, st *Store, email string) int64 {
	t.Helper()
	u, err := st.CreateUser(context.Background(), email, "hash")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func TestInboundAddressLifecycle(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if got, err := st.ActiveInboundAddress(ctx, u1); err != nil || got != nil {
		t.Fatalf("a user with no address must read as nil: got=%v err=%v", got, err)
	}

	a, err := st.CreateInboundAddress(ctx, u1, "lead-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if a.Provisioned() {
		t.Fatal("a freshly created address has no routing rule and must not read as provisioned")
	}

	// Until the routing rule exists the address cannot receive mail, so the
	// distinction has to survive a round trip.
	got, err := st.ActiveInboundAddress(ctx, u1)
	if err != nil || got == nil || got.Provisioned() {
		t.Fatalf("unprovisioned address round trip: got=%+v err=%v", got, err)
	}

	if err := st.SetInboundRuleID(ctx, u1, a.ID, "rule-123"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ActiveInboundAddress(ctx, u1)
	if err != nil || got == nil || !got.Provisioned() || got.CFRuleID != "rule-123" {
		t.Fatalf("provisioned address round trip: got=%+v err=%v", got, err)
	}
	if got.ProvisionedAt.IsZero() {
		t.Error("provisioning must be timestamped")
	}
}

func TestInboundAddressLookupAndRetirement(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	a, err := st.CreateInboundAddress(ctx, u1, "lead-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetInboundRuleID(ctx, u1, a.ID, "rule-1"); err != nil {
		t.Fatal(err)
	}

	found, err := st.LookupInboundAddress(ctx, "lead-bbbb")
	if err != nil || found == nil || found.UserID != u1 {
		t.Fatalf("lookup must resolve to the owner: got=%+v err=%v", found, err)
	}
	if miss, err := st.LookupInboundAddress(ctx, "lead-nope"); err != nil || miss != nil {
		t.Fatalf("unknown local part must read as nil, not error: got=%v err=%v", miss, err)
	}

	// Rotation: the old address must still RESOLVE (mail is in flight and the
	// user's forwarding rule still points at it) but must no longer be active.
	if err := st.RetireInboundAddresses(ctx, u1); err != nil {
		t.Fatal(err)
	}
	retired, err := st.LookupInboundAddress(ctx, "lead-bbbb")
	if err != nil || retired == nil {
		t.Fatalf("a retired address must still resolve so in-flight mail is attributable: got=%v err=%v", retired, err)
	}
	if retired.Active {
		t.Error("retired address must not be active")
	}
	if retired.RetiredAt.IsZero() {
		t.Error("retirement must be timestamped so the grace period can expire")
	}
	if cur, err := st.ActiveInboundAddress(ctx, u1); err != nil || cur != nil {
		t.Fatalf("after retirement the user has no active address: got=%v err=%v", cur, err)
	}
}

func TestRetiredAddressesReleasedAfterGrace(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	a, err := st.CreateInboundAddress(ctx, u1, "lead-cccc")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RetireInboundAddresses(ctx, u1); err != nil {
		t.Fatal(err)
	}

	// A cutoff before the retirement must not release the rule yet.
	past, err := st.RetiredInboundAddressesBefore(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(past) != 0 {
		t.Fatalf("grace period not elapsed, want 0 releasable, got %d", len(past))
	}

	due, err := st.RetiredInboundAddressesBefore(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != a.ID {
		t.Fatalf("want the retired address releasable, got %+v", due)
	}

	if err := st.DeleteInboundAddress(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if gone, err := st.LookupInboundAddress(ctx, "lead-cccc"); err != nil || gone != nil {
		t.Fatalf("deleted address must not resolve: got=%v err=%v", gone, err)
	}
}

func TestInboundAddressBudgetIsGlobal(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	// Routing rules are capped per domain by the mail provider, not per user,
	// so the budget check counts across all tenants.
	if _, err := st.CreateInboundAddress(ctx, u1, "lead-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInboundAddress(ctx, u2, "lead-2"); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountActiveInboundAddresses(ctx)
	if err != nil || n != 2 {
		t.Fatalf("want 2 active across tenants, got %d err=%v", n, err)
	}

	if err := st.RetireInboundAddresses(ctx, u1); err != nil {
		t.Fatal(err)
	}
	n, err = st.CountActiveInboundAddresses(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retirement must free budget: got %d err=%v", n, err)
	}
}

func TestInboundLocalPartIsGloballyUnique(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if _, err := st.CreateInboundAddress(ctx, u1, "lead-dupe"); err != nil {
		t.Fatal(err)
	}
	// Two tenants must never share an address: resolution is by local part
	// alone, so a collision would deliver one user's leads to another.
	if _, err := st.CreateInboundAddress(ctx, u2, "lead-dupe"); err == nil {
		t.Fatal("a duplicate local part across tenants must be rejected")
	}
}

func newEvent(userID int64, reason string) *IngestionEvent {
	return &IngestionEvent{
		UserID:      userID,
		Status:      EventQuarantined,
		Reason:      reason,
		Detail:      "Precedence: bulk",
		MessageID:   "<m1@example.com>",
		FromAddress: "news@example.com",
		FromName:    "Example News",
		Subject:     "Weekly roundup",
		ReceivedAt:  time.Now().UTC(),
		BodyExcerpt: "this week in example",
		BodyText:    "this week in example, at length",
	}
}

func TestIngestionEventsScopedByUser(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	mine := newEvent(u1, "machine_mail")
	theirs := newEvent(u2, "machine_mail")
	if err := st.InsertEvent(ctx, mine); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, theirs); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListEvents(ctx, u1, []EventStatus{EventQuarantined}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Fatalf("quarantine list must contain only the caller's rows, got %+v", list)
	}

	// Another tenant's event must read as absent, so handlers 404 and ids stay
	// non-enumerable across tenants.
	if got, err := st.GetEvent(ctx, u1, theirs.ID); err != nil || got != nil {
		t.Fatalf("cross-tenant GetEvent must read as absent: got=%v err=%v", got, err)
	}
	if err := st.SettleEvent(ctx, u1, theirs.ID, EventDismissed, 0); err == nil {
		t.Fatal("settling another tenant's event must fail")
	}
	still, err := st.GetEvent(ctx, u2, theirs.ID)
	if err != nil || still == nil || still.Status != EventQuarantined {
		t.Fatalf("the victim's row must be untouched: got=%+v err=%v", still, err)
	}
}

func TestSettleEventRescue(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	ev := newEvent(u1, "too_short")
	if err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleEvent(ctx, u1, ev.ID, EventRescued, 42); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEvent(ctx, u1, ev.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status != EventRescued || got.SubmissionID != 42 {
		t.Fatalf("rescue must record the submission it produced: got=%+v", got)
	}
	if got.SettledAt.IsZero() {
		t.Error("settling must stamp settled_at — it starts the retention clock")
	}

	// Settling twice must not silently succeed: the second caller would
	// otherwise believe it rescued something.
	if err := st.SettleEvent(ctx, u1, ev.ID, EventDismissed, 0); err == nil {
		t.Error("settling an already-settled event must fail")
	}
}

func TestCountEventsForBadge(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	for range 3 {
		if err := st.InsertEvent(ctx, newEvent(u1, "machine_mail")); err != nil {
			t.Fatal(err)
		}
	}
	dismissed := newEvent(u1, "machine_mail")
	if err := st.InsertEvent(ctx, dismissed); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleEvent(ctx, u1, dismissed.ID, EventDismissed, 0); err != nil {
		t.Fatal(err)
	}

	n, err := st.CountEvents(ctx, u1, EventQuarantined)
	if err != nil || n != 3 {
		t.Fatalf("want 3 outstanding, got %d err=%v", n, err)
	}
	if n, err := st.CountEvents(ctx, u2, EventQuarantined); err != nil || n != 0 {
		t.Fatalf("another tenant's badge must be 0, got %d err=%v", n, err)
	}
}

func TestPurgeIngestionBodiesKeepsExcerpt(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	ev := newEvent(u1, "machine_mail")
	if err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	// Unsettled events are not purged: the user has not dealt with them yet.
	n, err := st.PurgeIngestionBodies(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("unsettled events must survive the purge: purged=%d err=%v", n, err)
	}

	if err := st.SettleEvent(ctx, u1, ev.ID, EventDismissed, 0); err != nil {
		t.Fatal(err)
	}
	n, err = st.PurgeIngestionBodies(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("want 1 purged, got %d err=%v", n, err)
	}

	got, err := st.GetEvent(ctx, u1, ev.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.BodyText != "" {
		t.Error("body text must be purged")
	}
	if got.BodyExcerpt == "" {
		t.Error("the excerpt must survive so the quarantine list stays readable after a purge")
	}
}

func TestBlockedSenders(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if err := st.BlockSender(ctx, u1, "  Spam@Example.COM "); err != nil {
		t.Fatal(err)
	}
	// Blocking twice must be idempotent — it is a one-click action that a user
	// will inevitably hit twice.
	if err := st.BlockSender(ctx, u1, "spam@example.com"); err != nil {
		t.Fatalf("re-blocking must be a no-op, got %v", err)
	}
	got, err := st.BlockedSenders(ctx, u1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "spam@example.com" {
		t.Fatalf("patterns must be normalised and deduped, got %v", got)
	}
	if other, err := st.BlockedSenders(ctx, u2); err != nil || len(other) != 0 {
		t.Fatalf("blocklists are per-user, got %v err=%v", other, err)
	}

	if err := st.UnblockSender(ctx, u1, "SPAM@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.BlockedSenders(ctx, u1); len(got) != 0 {
		t.Fatalf("unblock must be case-insensitive too, got %v", got)
	}
}

func TestEmailsTodayCountsOnlyEmailSourced(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	midnight := time.Now().UTC().Truncate(24 * time.Hour)

	insert := func(hash string, src Source) {
		t.Helper()
		if _, err := st.Insert(ctx, &Submission{
			UserID: u1, ContentHash: hash, Status: StatusPending, Source: src,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("e1", SourceEmail)
	insert("e2", SourceEmail)
	insert("p1", SourcePaste)
	if _, err := st.Insert(ctx, &Submission{
		UserID: u2, ContentHash: "e3", Status: StatusPending, Source: SourceEmail,
	}); err != nil {
		t.Fatal(err)
	}

	// The cap exists to bound an LLM bill driven by mail the user does not
	// directly control, so pastes must not consume it.
	n, err := st.EmailsToday(ctx, u1, midnight)
	if err != nil || n != 2 {
		t.Fatalf("want 2 email-sourced today for u1, got %d err=%v", n, err)
	}
	if n, err := st.EmailsToday(ctx, u2, midnight); err != nil || n != 1 {
		t.Fatalf("caps are per-user, got %d err=%v", n, err)
	}

	// Tomorrow's window starts empty — the cap is a daily budget, not a total.
	if n, err := st.EmailsToday(ctx, u1, midnight.Add(24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("want the next day's budget empty, got %d err=%v", n, err)
	}
}

func TestFindByMessageIDSurvivesTheDayBoundary(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	// The content hash buckets by calendar day. A mail system redelivering the
	// same message just after midnight UTC hashes differently, so the
	// Message-ID lookup is the only thing standing between a retry and a
	// duplicate lead.
	yesterday := time.Now().UTC().Add(-20 * time.Hour)
	sub := &Submission{
		UserID: u1, ContentHash: ContentHash(u1, "hello", nil, yesterday),
		Status: StatusPending, Source: SourceEmail, MessageID: "<retry@mail>",
	}
	if _, err := st.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if ContentHash(u1, "hello", nil, yesterday) == ContentHash(u1, "hello", nil, now) {
		t.Skip("test run did not cross a UTC day boundary; the hash path already dedups")
	}

	found, err := st.FindByMessageID(ctx, u1, "<retry@mail>", now.Add(-messageIDWindowForTest))
	if err != nil || found == nil {
		t.Fatalf("redelivery across midnight must be caught by Message-ID: got=%v err=%v", found, err)
	}
}

const messageIDWindowForTest = 7 * 24 * time.Hour

func TestFindByMessageIDIsScopedAndBounded(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := st.Insert(ctx, &Submission{
		UserID: u1, ContentHash: "mid1", Status: StatusPending,
		Source: SourceEmail, MessageID: "<shared@mail>",
	}); err != nil {
		t.Fatal(err)
	}

	// Two tenants can legitimately be forwarded the same message; neither may
	// see the other's row.
	got, err := st.FindByMessageID(ctx, u2, "<shared@mail>", now.Add(-time.Hour))
	if err != nil || got != nil {
		t.Fatalf("Message-ID dedup must be per-user: got=%v err=%v", got, err)
	}

	// An empty Message-ID must never match: plenty of mail has none, and
	// treating "" as a key would collapse every such message into one lead.
	if got, err := st.FindByMessageID(ctx, u1, "", now.Add(-time.Hour)); err != nil || got != nil {
		t.Fatalf("empty Message-ID must not match anything: got=%v err=%v", got, err)
	}

	// Outside the window a deliberate re-forward is allowed through.
	if got, err := st.FindByMessageID(ctx, u1, "<shared@mail>", now.Add(time.Hour)); err != nil || got != nil {
		t.Fatalf("beyond the dedup window the message must be accepted again: got=%v err=%v", got, err)
	}
}

func TestQueueSeenAtAndCapturedSince(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	uid := mustUser(t, st, "away@test.example")

	if seen, err := st.QueueSeenAt(ctx, uid); err != nil || !seen.IsZero() {
		t.Fatalf("a user who has never opened the queue reads as zero: got=%v err=%v", seen, err)
	}

	mark := time.Now().UTC().Add(-time.Hour)
	if err := st.MarkQueueSeen(ctx, uid, mark); err != nil {
		t.Fatal(err)
	}
	seen, err := st.QueueSeenAt(ctx, uid)
	if err != nil || seen.IsZero() {
		t.Fatalf("queue_seen_at round trip: got=%v err=%v", seen, err)
	}

	if _, err := st.Insert(ctx, &Submission{
		UserID: uid, ContentHash: "away1", Status: StatusPending, Source: SourceEmail,
	}); err != nil {
		t.Fatal(err)
	}
	// A paste is not something that arrived "while you were away" — the user
	// was there, they pasted it.
	if _, err := st.Insert(ctx, &Submission{
		UserID: uid, ContentHash: "away2", Status: StatusPending, Source: SourcePaste,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := st.CountCapturedSince(ctx, uid, seen)
	if err != nil || n != 1 {
		t.Fatalf("want 1 captured since last seen, got %d err=%v", n, err)
	}
}

func TestSubmissionEmailFieldsRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	// submissionCols and scanSubmission are coupled positionally with nothing
	// enforcing it; this is the test that catches a mismatch.
	received := time.Now().UTC().Truncate(time.Second)
	sub := &Submission{
		UserID: u1, ContentHash: "roundtrip", Status: StatusPending,
		Source: SourceEmail, MessageID: "<rt@mail>", FromAddress: "ada@lumen.studio",
		Subject: "landing page", ReceivedAt: received,
	}
	if _, err := st.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, u1, sub.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Source != SourceEmail || got.MessageID != "<rt@mail>" ||
		got.FromAddress != "ada@lumen.studio" || got.Subject != "landing page" {
		t.Fatalf("email envelope did not survive the round trip: %+v", got)
	}
	if !got.ReceivedAt.Equal(received) {
		t.Errorf("received_at: want %v, got %v", received, got.ReceivedAt)
	}
}

func TestLegacySubmissionsReadAsPastes(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	// A row written before the source column existed must not read as email —
	// it would get an "Email" badge and a missing sender in the review pane.
	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO submissions (user_id, content_hash, status, created_at)
		VALUES (?, 'legacy', ?, ?)`,
		u1, StatusPending, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListByStatus(ctx, u1, StatusPending, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].Source != SourcePaste {
		t.Errorf("a row with no source must read as a paste, got %q", list[0].Source)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	// Opening the same file twice must be safe: the DDL and every ADD COLUMN
	// guard runs on every boot, and production restarts constantly.
	path := filepath.Join(t.TempDir(), "twice.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := first.CreateInboundAddress(ctx, u1, "lead-persist"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a migrated database must succeed: %v", err)
	}
	defer second.Close()
	got, err := second.LookupInboundAddress(ctx, "lead-persist")
	if err != nil || got == nil {
		t.Fatalf("data must survive the second migrate: got=%v err=%v", got, err)
	}
}

// TestDiscardFreesTheMessageIDToo mirrors the content-hash tombstone.
//
// Discard rewrites content_hash so the same content can genuinely be
// resubmitted. The Message-ID lookup has to agree, or an email lead the user
// discards can never be captured again: the hash is freed but the Message-ID
// goes on matching forever, and a second forward of that message vanishes with
// nothing for the user to find. The non-unique index in migrate() was chosen
// for exactly this reason; this is the half that makes it real.
func TestDiscardFreesTheMessageIDToo(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	sub := &Submission{
		UserID: u1, ContentHash: "mid-discard", Status: StatusPending,
		Source: SourceEmail, MessageID: "<again@mail>",
	}
	if _, err := st.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	// While it is live, a redelivery is correctly deduped.
	if got, err := st.FindByMessageID(ctx, u1, "<again@mail>", since); err != nil || got == nil {
		t.Fatalf("a live submission must still dedup redelivery: %v %v", got, err)
	}

	if err := st.Discard(ctx, u1, sub.ID); err != nil {
		t.Fatal(err)
	}

	// Once discarded, forwarding it again must get through.
	got, err := st.FindByMessageID(ctx, u1, "<again@mail>", since)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("a discarded lead still blocks its Message-ID (matched id %d), so re-forwarding it would silently vanish", got.ID)
	}
}
