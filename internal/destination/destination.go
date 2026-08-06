// Package destination defines the seam between a confirmed lead and wherever
// it gets written. It is deliberately free of any provider's vocabulary: the
// Google Sheets writer and the Notion writer both implement Writer, and the
// confirm path picks between them by the user's destination type without
// knowing anything else about either.
//
// The unit of exchange is a Lead — ordered (schema field, value) pairs — rather
// than a row of strings. A sheet only needs the values in column order, but a
// Notion database has typed properties, so the field a value came from has to
// survive the trip.
package destination

import "context"

// Type identifies a destination kind. It is persisted in the destinations
// table, so the string values are part of the on-disk format.
type Type string

const (
	TypeGoogleSheet Type = "google_sheet"
	TypeNotion      Type = "notion"
)

// Valid reports whether t is a destination type this build knows how to write.
func (t Type) Valid() bool {
	return t == TypeGoogleSheet || t == TypeNotion
}

// Cell is one schema field and its confirmed value. Field is a name from
// config/schema.json (plus the synthetic "flags").
type Cell struct {
	Field string
	Value string
}

// Lead is one confirmed lead. Cells are in the schema's column order, which is
// what a row-oriented destination writes directly; a property-oriented
// destination looks cells up by Field instead.
type Lead struct {
	Cells []Cell
}

// Value returns the value for a field name, and whether the field is present.
func (l Lead) Value(field string) (string, bool) {
	for _, c := range l.Cells {
		if c.Field == field {
			return c.Value, true
		}
	}
	return "", false
}

// Values returns the cell values in order — the row form, for row-oriented
// destinations.
func (l Lead) Values() []string {
	out := make([]string, len(l.Cells))
	for i, c := range l.Cells {
		out[i] = c.Value
	}
	return out
}

// Result describes what actually landed at the destination.
//
// Dropped names the schema fields the destination could not accept — a Notion
// database with no property mapped to "notes", say. A dropped field is never a
// silent loss: the write succeeds with everything that mapped, and the caller
// surfaces the dropped names to the user. It is always empty for a sheet, where
// every column exists by construction.
type Result struct {
	Dropped []string
	// URL links to the written row or page when the destination exposes one.
	URL string
}

// Writer writes confirmed leads to one user's destination and reads the tail
// back for the preview strip.
type Writer interface {
	// Write appends one lead. It returns an error only when nothing was
	// written; a partial write (some fields dropped) is a success carrying
	// Result.Dropped.
	Write(ctx context.Context, lead Lead) (Result, error)
	// Recent returns up to n most recent leads as rows in schema column
	// order, newest last, for the preview strip.
	Recent(ctx context.Context, n int) ([][]string, error)
}
