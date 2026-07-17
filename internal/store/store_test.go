package store

import (
	"context"
	"database/sql"
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

func TestGetUpdateDelete(t *testing.T) {
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

	if err := st.Delete(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := st.Get(ctx, sub.ID)
	if err != nil || gone != nil {
		t.Fatalf("expected nil after delete, got %+v err=%v", gone, err)
	}
	// Deleting frees the hash for genuine resubmission.
	dup, err := st.Insert(ctx, &Submission{ContentHash: "a", Status: StatusPending})
	if err != nil || dup {
		t.Fatalf("hash should be free after delete: dup=%v err=%v", dup, err)
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
	// New columns usable on the migrated table.
	if _, err := st.Insert(ctx, &Submission{ContentHash: "new", Status: StatusPending, InputText: "t"}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}
