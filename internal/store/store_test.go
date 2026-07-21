package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// u1 / u2 are two distinct tenants used throughout the store tests.
const (
	u1 int64 = 1
	u2 int64 = 2
)

func TestDedupHashDayBucket(t *testing.T) {
	d1 := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 8, 22, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)
	if ContentHash(u1, "hi", nil, d1) != ContentHash(u1, "hi", nil, d2) {
		t.Fatal("same content same user same day must hash equal")
	}
	if ContentHash(u1, "hi", nil, d1) == ContentHash(u1, "hi", nil, d3) {
		t.Fatal("same content different day must hash differently")
	}
	if ContentHash(u1, "hi", nil, d1) == ContentHash(u1, "hi", []byte{1}, d1) {
		t.Fatal("image bytes must affect the hash")
	}
	if ContentHash(u1, "hi", nil, d1) == ContentHash(u2, "hi", nil, d1) {
		t.Fatal("different users must hash identical content differently")
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInsertAndDuplicate(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{UserID: u1, ContentHash: "abc", Status: StatusWritten, Extraction: []byte(`{"need":"x"}`)}
	dup, err := st.Insert(ctx, sub)
	if err != nil || dup {
		t.Fatalf("first insert: dup=%v err=%v", dup, err)
	}
	if sub.ID == 0 {
		t.Fatal("insert must set the id")
	}
	dup, err = st.Insert(ctx, sub)
	if err != nil || !dup {
		t.Fatalf("second insert should be duplicate: dup=%v err=%v", dup, err)
	}

	found, err := st.FindByHash(ctx, u1, "abc")
	if err != nil || found == nil || found.Status != StatusWritten {
		t.Fatalf("find: %+v err=%v", found, err)
	}
	missing, err := st.FindByHash(ctx, u1, "nope")
	if err != nil || missing != nil {
		t.Fatalf("expected nil for unknown hash, got %+v err=%v", missing, err)
	}
}

func TestInputRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{
		UserID: u1, ContentHash: "img", Status: StatusPending,
		InputText: "full pasted text", InputImage: []byte{0x89, 0x50}, InputImageType: "image/png",
	}
	if _, err := st.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, u1, sub.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if got.InputText != "full pasted text" || got.InputImageType != "image/png" || len(got.InputImage) != 2 {
		t.Fatalf("input fields not round-tripped: %+v", got)
	}
	if got.UserID != u1 {
		t.Fatalf("user_id not round-tripped: %d", got.UserID)
	}
}

func TestGetUpdateDiscard(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{UserID: u1, ContentHash: "a", Status: StatusPending, Extraction: []byte(`{}`)}
	st.Insert(ctx, sub)

	if err := st.Update(ctx, u1, sub.ID, StatusWritten, []byte(`{"need":"edited"}`), ""); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, u1, sub.ID)
	if got.Status != StatusWritten || string(got.Extraction) != `{"need":"edited"}` {
		t.Fatalf("update not applied: %+v", got)
	}

	if err := st.Update(ctx, u1, 9999, StatusWritten, nil, ""); err == nil {
		t.Fatal("update of unknown id must error")
	}

	if err := st.Discard(ctx, u1, sub.ID); err != nil {
		t.Fatal(err)
	}
	// Soft delete: the row survives with a tombstoned hash.
	kept, err := st.Get(ctx, u1, sub.ID)
	if err != nil || kept == nil {
		t.Fatalf("row must survive discard, got %+v err=%v", kept, err)
	}
	if kept.Status != StatusDiscarded {
		t.Fatalf("status = %s, want discarded", kept.Status)
	}
	wantHash := fmt.Sprintf("discarded:%d:a", sub.ID)
	if kept.ContentHash != wantHash {
		t.Fatalf("hash = %s, want %s", kept.ContentHash, wantHash)
	}
	if found, _ := st.FindByHash(ctx, u1, "a"); found != nil {
		t.Fatalf("original hash must be freed, found %+v", found)
	}
	// Discarding frees the hash for genuine resubmission.
	dup, err := st.Insert(ctx, &Submission{UserID: u1, ContentHash: "a", Status: StatusPending})
	if err != nil || dup {
		t.Fatalf("hash should be free after discard: dup=%v err=%v", dup, err)
	}
	// A second discard is a no-op: no double tombstone.
	if err := st.Discard(ctx, u1, sub.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Get(ctx, u1, sub.ID)
	if again.ContentHash != wantHash {
		t.Fatalf("double discard rewrote the hash: %s", again.ContentHash)
	}
}

func TestListAndCountByStatuses(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	st.Insert(ctx, &Submission{UserID: u1, ContentHash: "a", Status: StatusPending})
	st.Insert(ctx, &Submission{UserID: u1, ContentHash: "b", Status: StatusWritten})
	st.Insert(ctx, &Submission{UserID: u1, ContentHash: "c", Status: StatusFailedWrite})
	st.Insert(ctx, &Submission{UserID: u1, ContentHash: "d", Status: StatusPending})

	subs, err := st.ListByStatuses(ctx, u1, []Status{StatusPending, StatusFailedWrite}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 3 {
		t.Fatalf("expected 3 queued, got %d", len(subs))
	}
	if subs[0].ContentHash != "d" {
		t.Fatalf("expected newest first, got %s", subs[0].ContentHash)
	}

	n, err := st.CountByStatus(ctx, u1, StatusPending, StatusFailedWrite)
	if err != nil || n != 3 {
		t.Fatalf("count = %d err=%v, want 3", n, err)
	}
	written, err := st.ListByStatus(ctx, u1, StatusWritten, 10)
	if err != nil || len(written) != 1 {
		t.Fatalf("written = %d err=%v", len(written), err)
	}
}

// TestSubmissionsIsolatedByUser is the core multi-tenant guarantee: one user
// can never read, update, discard, or list another user's submissions.
func TestSubmissionsIsolatedByUser(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	// Both users submit and both rows persist (no cross-user hash collision).
	a := &Submission{UserID: u1, ContentHash: ContentHash(u1, "same text", nil, time.Now()), Status: StatusPending}
	b := &Submission{UserID: u2, ContentHash: ContentHash(u2, "same text", nil, time.Now()), Status: StatusPending}
	if _, err := st.Insert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if dup, err := st.Insert(ctx, b); err != nil || dup {
		t.Fatalf("second user's identical text must not dedupe: dup=%v err=%v", dup, err)
	}

	// Reads are scoped: user 2 cannot see user 1's row (reads as absent → 404).
	if got, _ := st.Get(ctx, u2, a.ID); got != nil {
		t.Fatalf("user 2 must not read user 1's submission, got %+v", got)
	}
	if got, _ := st.Get(ctx, u1, a.ID); got == nil {
		t.Fatal("owner must still read their own submission")
	}

	// Updates are scoped: user 2 updating user 1's id is "not found".
	if err := st.Update(ctx, u2, a.ID, StatusWritten, nil, ""); err == nil {
		t.Fatal("cross-user update must fail")
	}
	if got, _ := st.Get(ctx, u1, a.ID); got.Status != StatusPending {
		t.Fatalf("cross-user update leaked: status=%s", got.Status)
	}

	// Discards are scoped: user 2 discarding user 1's id is a no-op.
	if err := st.Discard(ctx, u2, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, u1, a.ID); got.Status == StatusDiscarded {
		t.Fatal("cross-user discard leaked")
	}

	// Lists and counts are scoped.
	list1, _ := st.ListByStatuses(ctx, u1, []Status{StatusPending}, 10)
	list2, _ := st.ListByStatuses(ctx, u2, []Status{StatusPending}, 10)
	if len(list1) != 1 || list1[0].ID != a.ID {
		t.Fatalf("user 1 list wrong: %+v", list1)
	}
	if len(list2) != 1 || list2[0].ID != b.ID {
		t.Fatalf("user 2 list wrong: %+v", list2)
	}
	if n, _ := st.CountByStatus(ctx, u1, StatusPending); n != 1 {
		t.Fatalf("user 1 count = %d, want 1", n)
	}

	// FindByHash is scoped: user 1's hash is invisible to user 2.
	if got, _ := st.FindByHash(ctx, u2, a.ContentHash); got != nil {
		t.Fatal("cross-user FindByHash leaked")
	}
}

func TestPurgeInputs(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	insert := func(hash string, status Status) *Submission {
		t.Helper()
		sub := &Submission{
			UserID: u1, ContentHash: hash, Status: status, Extraction: []byte(`{"need":"x"}`),
			InputExcerpt: "excerpt", InputText: "full text", InputImage: []byte{1, 2}, InputImageType: "image/png",
		}
		if _, err := st.Insert(ctx, sub); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	oldWritten := insert("w-old", StatusWritten)
	oldDiscarded := insert("d-old", StatusPending)
	freshWritten := insert("w-new", StatusWritten)
	pending := insert("p", StatusPending)

	if err := st.Discard(ctx, u1, oldDiscarded.ID); err != nil {
		t.Fatal(err)
	}
	// Backdate the two "old" settled rows past the cutoff.
	past := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	for _, id := range []int64{oldWritten.ID, oldDiscarded.ID} {
		if _, err := st.db.Exec(`UPDATE submissions SET settled_at = ? WHERE id = ?`, past, id); err != nil {
			t.Fatal(err)
		}
	}
	// freshWritten needs a settled_at too (Insert doesn't stamp it); keep it recent.
	if _, err := st.db.Exec(`UPDATE submissions SET settled_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), freshWritten.ID); err != nil {
		t.Fatal(err)
	}

	n, err := st.PurgeInputs(ctx, time.Now().UTC().Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("purged %d rows, want 2", n)
	}

	for _, id := range []int64{oldWritten.ID, oldDiscarded.ID} {
		got, _ := st.Get(ctx, u1, id)
		if got.InputText != "" || len(got.InputImage) != 0 || got.InputImageType != "" {
			t.Fatalf("row %d: originals not purged: %+v", id, got)
		}
		if got.InputExcerpt != "excerpt" || string(got.Extraction) != `{"need":"x"}` {
			t.Fatalf("row %d: excerpt/extraction must survive the purge: %+v", id, got)
		}
	}
	for _, id := range []int64{freshWritten.ID, pending.ID} {
		got, _ := st.Get(ctx, u1, id)
		if got.InputText == "" || len(got.InputImage) == 0 {
			t.Fatalf("row %d: must not be purged: %+v", id, got)
		}
	}

	// Idempotent: nothing left to purge.
	if n, _ := st.PurgeInputs(ctx, time.Now().UTC().Add(-14*24*time.Hour)); n != 0 {
		t.Fatalf("second purge touched %d rows, want 0", n)
	}
}

func TestSettledAtStamping(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{UserID: u1, ContentHash: "s", Status: StatusPending}
	st.Insert(ctx, sub)
	if got, _ := st.Get(ctx, u1, sub.ID); !got.SettledAt.IsZero() {
		t.Fatalf("pending row must have no settled_at: %v", got.SettledAt)
	}
	if err := st.Update(ctx, u1, sub.ID, StatusFailedWrite, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, u1, sub.ID); !got.SettledAt.IsZero() {
		t.Fatalf("failed_write must not stamp settled_at: %v", got.SettledAt)
	}
	if err := st.Update(ctx, u1, sub.ID, StatusWritten, nil, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, u1, sub.ID)
	if got.SettledAt.IsZero() {
		t.Fatal("written must stamp settled_at")
	}
	first := got.SettledAt
	// A later update must not move the settled clock.
	if err := st.Update(ctx, u1, sub.ID, StatusWritten, nil, ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Get(ctx, u1, sub.ID); !again.SettledAt.Equal(first) {
		t.Fatalf("settled_at moved: %v -> %v", first, again.SettledAt)
	}
}

// TestMigrateOldSchema opens a database created before the review-pane
// rework (no input columns, no user_id, needs_review rows) and proves Open
// upgrades it — including adding user_id (backfilled NULL, invisible to any
// real tenant).
func TestMigrateOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE submissions (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash  TEXT NOT NULL UNIQUE,
			status        TEXT NOT NULL,
			extraction    TEXT,
			flags         TEXT,
			input_excerpt TEXT,
			error         TEXT,
			created_at    TEXT NOT NULL
		);
		INSERT INTO submissions (content_hash, status, extraction, created_at)
		VALUES ('old1', 'needs_review', '{"need":"x"}', '2026-07-01T00:00:00Z'),
		       ('old2', 'written', '{}', '2026-07-01T00:00:00Z');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open over old schema: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// The user_id column now exists and legacy rows are NULL — so no real
	// tenant (nor user 0, since NULL != 0) can read them; they are orphaned
	// and invisible via the user-scoped queries. Assert against the raw table.
	var nullCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM submissions WHERE user_id IS NULL`).Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 2 {
		t.Fatalf("legacy rows should have NULL user_id, got %d", nullCount)
	}
	if got, _ := st.FindByHash(ctx, u1, "old1"); got != nil {
		t.Fatalf("legacy rows must be invisible to a real tenant, got %+v", got)
	}

	// needs_review collapses to pending; the written row is untouched and gets
	// settled_at backfilled from created_at; the pending row does not.
	rawRow := func(hash string) (status, settledAt, createdAt string) {
		t.Helper()
		var settled sql.NullString
		if err := st.db.QueryRow(
			`SELECT status, settled_at, created_at FROM submissions WHERE content_hash = ?`, hash).
			Scan(&status, &settled, &createdAt); err != nil {
			t.Fatalf("raw row %s: %v", hash, err)
		}
		return status, settled.String, createdAt
	}
	if status, settled, _ := rawRow("old1"); status != string(StatusPending) || settled != "" {
		t.Fatalf("old1: status=%s settled=%q, want pending + no settled_at", status, settled)
	}
	if status, settled, created := rawRow("old2"); status != string(StatusWritten) || settled != created {
		t.Fatalf("old2: status=%s settled=%q created=%q, want written + backfilled settled_at", status, settled, created)
	}

	// New columns are usable on the migrated table, with a real user.
	fresh := &Submission{UserID: u1, ContentHash: "new", Status: StatusPending, InputText: "t"}
	if _, err := st.Insert(ctx, fresh); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if got, _ := st.Get(ctx, u1, fresh.ID); got == nil || got.InputText != "t" {
		t.Fatalf("new row not readable by its owner: %+v", got)
	}
}
