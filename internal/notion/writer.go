package notion

import (
	"context"
	"errors"
	"sort"

	"github.com/EOEboh/ziga-data/internal/destination"
)

// Writer writes confirmed leads as pages in one Notion data source,
// implementing destination.Writer.
type Writer struct {
	client       *Client
	dataSourceID string
	mapping      Mapping
	// columns is the schema's column order, so Recent can render Notion pages
	// back into the row shape the preview strip expects.
	columns []string
	// selectOptions caches the known options of select properties, so a value
	// rejected as unknown can be added to the schema and retried.
	selectOptions map[string][]string
}

// NewWriter builds a writer for a user's Notion destination.
func NewWriter(client *Client, dataSourceID string, mapping Mapping, columns []string) *Writer {
	return &Writer{
		client:        client,
		dataSourceID:  dataSourceID,
		mapping:       mapping,
		columns:       columns,
		selectOptions: map[string][]string{},
	}
}

// Write creates one page from a lead.
//
// A field is dropped rather than fatal when the database has no property for
// it, or when its value cannot be represented in the mapped property's type.
// The page is still created with everything that does map, and the dropped
// names come back in the Result so the user is told — losing a field silently
// would be worse than the write failing.
func (w *Writer) Write(ctx context.Context, lead destination.Lead) (destination.Result, error) {
	props := map[string]any{}
	var dropped []string
	// selects records which mapped property each written select value went to,
	// so an unknown-option rejection can be repaired precisely.
	selects := map[string]string{}

	for _, cell := range lead.Cells {
		target, mapped := w.mapping[cell.Field]
		if !mapped {
			// No property for this field. An empty value is not a loss, so
			// only report fields that actually carried something.
			if cell.Value != "" {
				dropped = append(dropped, cell.Field)
			}
			continue
		}
		value, ok := buildValue(target.Type, cell.Value)
		if !ok {
			if cell.Value != "" {
				dropped = append(dropped, cell.Field)
			}
			continue
		}
		props[target.Name] = value
		if target.Type == TypeSelect || target.Type == TypeMultiSelect {
			selects[target.Name] = cell.Value
		}
	}

	// Nothing mapped at all: the page would be blank, which is a real failure
	// rather than a partial success.
	if len(props) == 0 {
		return destination.Result{Dropped: dropped},
			errors.New("notion: none of the lead's fields map to a property in this database")
	}

	url, err := w.client.CreatePage(ctx, w.dataSourceID, props)
	if err != nil && IsValidation(err) && len(selects) > 0 {
		// A validation error with select values in play is most likely an
		// option Notion has not seen. Add the options and retry once. This is
		// correct whether or not Notion creates options implicitly, and it
		// costs nothing when values are already known.
		if added := w.addMissingSelectOptions(ctx, selects); added {
			url, err = w.client.CreatePage(ctx, w.dataSourceID, props)
		}
	}
	if err != nil {
		return destination.Result{}, err
	}

	sort.Strings(dropped)
	return destination.Result{Dropped: dropped, URL: url}, nil
}

// addMissingSelectOptions appends each written select value to its property's
// schema. It reports whether anything changed, so a retry only happens when a
// repair was actually attempted.
func (w *Writer) addMissingSelectOptions(ctx context.Context, selects map[string]string) bool {
	ds, err := w.client.GetDataSource(ctx, w.dataSourceID)
	if err != nil {
		return false
	}
	changed := false
	for propName, value := range selects {
		p, ok := ds.Property(propName)
		if !ok {
			continue
		}
		if contains(p.SelectOptions, value) {
			continue // the option exists; the rejection was something else
		}
		if err := w.client.AddSelectOption(ctx, w.dataSourceID, p, value); err != nil {
			continue
		}
		w.selectOptions[propName] = append(p.SelectOptions, value)
		changed = true
	}
	return changed
}

// Recent returns the most recent pages as rows in schema column order, for the
// preview strip. Columns with no mapped property render empty.
func (w *Writer) Recent(ctx context.Context, n int) ([][]string, error) {
	pages, err := w.client.QueryRecent(ctx, w.dataSourceID, n)
	if err != nil {
		return nil, err
	}
	rows := make([][]string, 0, len(pages))
	// Notion returns newest first; the preview strip expects oldest first.
	for i := len(pages) - 1; i >= 0; i-- {
		row := make([]string, len(w.columns))
		for j, col := range w.columns {
			if target, ok := w.mapping[col]; ok {
				row[j] = pages[i][target.Name]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
