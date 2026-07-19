package sheets

import (
	"context"
	"fmt"
)

// LastRows returns the last n data rows of the configured tab for the
// preview strip. In header mode the first row is skipped; an empty tab (or
// one holding only the header) yields no rows. Rows come back ragged (the
// API trims trailing empty cells); the caller pads to its column count.
func (w *Writer) LastRows(ctx context.Context, n int) ([][]string, error) {
	resp, err := w.svc.Spreadsheets.Values.Get(w.sheetID, w.tab).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("sheets read: %w", err)
	}
	rows := resp.Values
	if w.header != nil && len(rows) > 0 {
		rows = rows[1:] // header
	}
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, len(r))
		for i, cell := range r {
			row[i] = fmt.Sprint(cell)
		}
		out = append(out, row)
	}
	return out, nil
}
