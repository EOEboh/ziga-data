package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestDedupHashDayBucket(t *testing.T) {
	d1 := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 8, 22, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)
	if ContentHash("hi", nil, d1) != ContentHash("hi", nil, d2) {
		t.Fatal("same content same day must hash equal")
	}
	if ContentHash("hi", nil, d1) == ContentHash("hi", nil, d3) {
		t.Fatal("same content different day must hash differently")
	}
	if ContentHash("hi", nil, d1) == ContentHash("hi", []byte{1}, d1) {
		t.Fatal("image bytes must affect the hash")
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

	sub := &Submission{ContentHash: "abc", Status: StatusWritten, Extraction: []byte(`{"need":"x"}`)}
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

	found, err := st.FindByHash(ctx, "abc")
	if err != nil || found == nil || found.Status != StatusWritten {
		t.Fatalf("find: %+v err=%v", found, err)
	}
	missing, err := st.FindByHash(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("expected nil for unknown hash, got %+v err=%v", missing, err)
	}
}

func TestInputRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{
		ContentHash: "img", Status: StatusPending,
		InputText: "full pasted text", InputImage: []byte{0x89, 0x50}, InputImageType: "image/png",
	}
	if _, err := st.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, sub.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if got.InputText != "full pasted text" || got.InputImageType != "image/png" || len(got.InputImage) != 2 {
		t.Fatalf("input fields not round-tripped: %+v", got)
	}
}

func TestGetUpdateDiscard(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	sub := &Submission{ContentHash: "a", Status: StatusPending, Extraction: []byte(`{}`)}
	st.Insert(ctx, sub)

	if err := st.Update(ctx, sub.ID, StatusWritten, []byte(`{"need":"edited"}`), ""); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, sub.ID)
	if got.Status != StatusWritten || string(got.Extraction) != `{"need":"edited"}` {
		t.Fatalf("update not applied: %+v", got)
	}

	if err := st.Update(ctx, 9999, StatusWritten, nil, ""); err == nil {
		t.Fatal("update of unknown id must error")
	}

	if err := st.Discard(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	// Soft delete: the row survives with a tombstoned hash.
	kept, err := st.Get(ctx, sub.ID)
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
	if found, _ := st.FindByHash(ctx, "a"); found != nil {
		t.Fatalf("original hash must be freed, found %+v", found)
	}
	// Discarding frees the hash for genuine resubmission.
	dup, err := st.Insert(ctx, &Submission{ContentHash: "a", Status: StatusPending})
	if err != nil || dup {
		t.Fatalf("hash should be free after discard: dup=%v err=%v", dup, err)
	}
	// A second discard is a no-op: no double tombstone.
	if err := st.Discard(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Get(ctx, sub.ID)
	if again.ContentHash != wantHash {
		t.Fatalf("double discard rewrote the hash: %s", again.ContentHash)
	}
}

func TestListAndCountByStatuses(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	st.Insert(ctx, &Submission{ContentHash: "a", Status: StatusPending})
	st.Insert(ctx, &Submission{ContentHash: "b", Status: StatusWritten})
	st.Insert(ctx, &Submission{ContentHash: "c", Status: StatusFailedWrite})
	st.Insert(ctx, &Submission{ContentHash: "d", Status: StatusPending})

	subs, err := st.ListByStatuses(ctx, []Status{StatusPending, StatusFailedWrite}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 3 {
		t.Fatalf("expected 3 queued, got %d", len(subs))
	}
	if subs[0].ContentHash != "d" {
		t.Fatalf("expected newest first, got %s", subs[0].ContentHash)
	}

	n, err := st.CountByStatus(ctx, StatusPending, StatusFailedWrite)
	if err != nil || n != 3 {
		t.Fatalf("count = %d err=%v, want 3", n, err)
	}
	written, err := st.ListByStatus(ctx, StatusWritten, 10)
	if err != nil || len(written) != 1 {
		t.Fatalf("written = %d err=%v", len(written), err)
	}
}

func TestPurgeInputs(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	insert := func(hash string, status Status) *Submission {
		t.Helper()
		sub := &Submission{
			ContentHash: hash, Status: status, Extraction: []byte(`{"need":"x"}`),
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

	if err := st.Discard(ctx, oldDiscarded.ID); err != nil {
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
		got, _ := st.Get(ctx, id)
		if got.InputText != "" || len(got.InputImage) != 0 || got.InputImageType != "" {
			t.Fatalf("row %d: originals not purged: %+v", id, got)
		}
		if got.InputExcerpt != "excerpt" || string(got.Extraction) != `{"need":"x"}` {
			t.Fatalf("row %d: excerpt/extraction must survive the purge: %+v", id, got)
		}
	}
	for _, id := range []int64{freshWritten.ID, pending.ID} {
		got, _ := st.Get(ctx, id)
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

	sub := &Submission{ContentHash: "s", Status: StatusPending}
	st.Insert(ctx, sub)
	if got, _ := st.Get(ctx, sub.ID); !got.SettledAt.IsZero() {
		t.Fatalf("pending row must have no settled_at: %v", got.SettledAt)
	}
	if err := st.Update(ctx, sub.ID, StatusFailedWrite, nil, "boom"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, sub.ID); !got.SettledAt.IsZero() {
		t.Fatalf("failed_write must not stamp settled_at: %v", got.SettledAt)
	}
	if err := st.Update(ctx, sub.ID, StatusWritten, nil, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, sub.ID)
	if got.SettledAt.IsZero() {
		t.Fatal("written must stamp settled_at")
	}
	first := got.SettledAt
	// A later update must not move the settled clock.
	if err := st.Update(ctx, sub.ID, StatusWritten, nil, ""); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Get(ctx, sub.ID); !again.SettledAt.Equal(first) {
		t.Fatalf("settled_at moved: %v -> %v", first, again.SettledAt)
	}
}

// TestMigrateOldSchema opens a database created before the review-pane
// rework (no input columns, needs_review rows) and proves Open upgrades it.
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

	sub, err := st.FindByHash(ctx, "old1")
	if err != nil || sub == nil {
		t.Fatalf("find old row: %+v err=%v", sub, err)
	}
	if sub.Status != StatusPending {
		t.Fatalf("needs_review not migrated to pending: %s", sub.Status)
	}
	written, _ := st.FindByHash(ctx, "old2")
	if written.Status != StatusWritten {
		t.Fatalf("written row must be untouched: %s", written.Status)
	}
	// Legacy settled rows get settled_at backfilled from created_at so the
	// retention purge eventually reaches them; unsettled rows stay unset.
	if written.SettledAt.IsZero() || !written.SettledAt.Equal(written.CreatedAt) {
		t.Fatalf("settled_at not backfilled from created_at: %v vs %v", written.SettledAt, written.CreatedAt)
	}
	if !sub.SettledAt.IsZero() {
		t.Fatalf("pending row must not get settled_at backfilled: %v", sub.SettledAt)
	}
	// New columns usable on the migrated table.
	if _, err := st.Insert(ctx, &Submission{ContentHash: "new", Status: StatusPending, InputText: "t"}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}
