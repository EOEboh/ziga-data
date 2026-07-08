package store

import (
	"context"
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

func TestInsertAndDuplicate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	sub := &Submission{ContentHash: "abc", Status: StatusWritten, Extraction: []byte(`{"need":"x"}`)}
	dup, err := st.Insert(ctx, sub)
	if err != nil || dup {
		t.Fatalf("first insert: dup=%v err=%v", dup, err)
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

func TestListByStatus(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	st.Insert(ctx, &Submission{ContentHash: "a", Status: StatusNeedsReview})
	st.Insert(ctx, &Submission{ContentHash: "b", Status: StatusWritten})
	st.Insert(ctx, &Submission{ContentHash: "c", Status: StatusNeedsReview})

	subs, err := st.ListByStatus(ctx, StatusNeedsReview, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 needs_review, got %d", len(subs))
	}
	if subs[0].ContentHash != "c" {
		t.Fatalf("expected newest first, got %s", subs[0].ContentHash)
	}
}
