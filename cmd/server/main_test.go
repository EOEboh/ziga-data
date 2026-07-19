package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
}

// The dry-run writer mirrors the real sheets writer's header contract, so
// the full flow (incl. HEADER_ROW behavior) is exercisable without a sheet.
func TestDryRunWriterHeaderMode(t *testing.T) {
	ctx := context.Background()
	d := &dryRunWriter{log: testLogger(), header: []string{"date", "name"}}

	// Empty sheet: nothing to preview yet.
	rows, err := d.LastRows(ctx, 3)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty sheet: rows=%v err=%v", rows, err)
	}

	// First append writes the header row before the data row.
	if err := d.Append(ctx, []string{"2026-07-18", "Jane"}); err != nil {
		t.Fatal(err)
	}
	if len(d.rows) != 2 || d.rows[0][0] != "date" {
		t.Fatalf("header not written first: %v", d.rows)
	}
	if err := d.Append(ctx, []string{"2026-07-18", "Ada"}); err != nil {
		t.Fatal(err)
	}
	if len(d.rows) != 3 {
		t.Fatalf("header must be written only once: %v", d.rows)
	}

	// LastRows skips the header and returns data rows only.
	rows, err = d.LastRows(ctx, 3)
	if err != nil || len(rows) != 2 || rows[0][1] != "Jane" || rows[1][1] != "Ada" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestDryRunWriterNoHeaderMode(t *testing.T) {
	ctx := context.Background()
	d := &dryRunWriter{log: testLogger()}

	if err := d.Append(ctx, []string{"2026-07-18", "Jane"}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.LastRows(ctx, 3)
	if err != nil || len(rows) != 1 || rows[0][1] != "Jane" {
		t.Fatalf("no-header mode must not swallow the first row: rows=%v err=%v", rows, err)
	}
}
