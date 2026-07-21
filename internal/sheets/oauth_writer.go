package sheets

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// NewOAuthWriter builds a Writer that authenticates as the user via their OAuth
// token source (drive.file scope), rather than a shared service account. This
// is the multi-tenant path: each user writes to their own spreadsheet. Extra
// options (e.g. a test endpoint) are appended.
func NewOAuthWriter(ctx context.Context, ts oauth2.TokenSource, sheetID, tab string, header []string, opts ...option.ClientOption) (*Writer, error) {
	svc, err := newService(ctx, ts, opts...)
	if err != nil {
		return nil, err
	}
	return &Writer{svc: svc, sheetID: sheetID, tab: tab, header: header}, nil
}

func newService(ctx context.Context, ts oauth2.TokenSource, opts ...option.ClientOption) (*sheets.Service, error) {
	all := append([]option.ClientOption{option.WithTokenSource(ts)}, opts...)
	svc, err := sheets.NewService(ctx, all...)
	if err != nil {
		return nil, fmt.Errorf("sheets service: %w", err)
	}
	return svc, nil
}

// CreateSpreadsheet creates a new spreadsheet titled `title` with a single tab
// named `tab`, writes the header row when provided, and returns the new
// spreadsheet id. Works under the drive.file scope because the app is the
// creator of the file.
func CreateSpreadsheet(ctx context.Context, ts oauth2.TokenSource, title, tab string, header []string, opts ...option.ClientOption) (string, error) {
	svc, err := newService(ctx, ts, opts...)
	if err != nil {
		return "", err
	}
	created, err := svc.Spreadsheets.Create(&sheets.Spreadsheet{
		Properties: &sheets.SpreadsheetProperties{Title: title},
		Sheets:     []*sheets.Sheet{{Properties: &sheets.SheetProperties{Title: tab}}},
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create spreadsheet: %w", err)
	}
	if header != nil {
		vals := make([]any, len(header))
		for i, h := range header {
			vals[i] = h
		}
		if _, err := svc.Spreadsheets.Values.Append(created.SpreadsheetId, tab, &sheets.ValueRange{Values: [][]any{vals}}).
			ValueInputOption("RAW").InsertDataOption("INSERT_ROWS").Context(ctx).Do(); err != nil {
			return "", fmt.Errorf("write header row: %w", err)
		}
	}
	return created.SpreadsheetId, nil
}

// FirstSheetTitle returns the title of a spreadsheet's first tab, so an
// attached (Picker-selected) spreadsheet's appends target a real tab.
func FirstSheetTitle(ctx context.Context, ts oauth2.TokenSource, spreadsheetID string, opts ...option.ClientOption) (string, error) {
	svc, err := newService(ctx, ts, opts...)
	if err != nil {
		return "", err
	}
	ss, err := svc.Spreadsheets.Get(spreadsheetID).Fields("sheets.properties.title").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("read spreadsheet: %w", err)
	}
	if len(ss.Sheets) == 0 || ss.Sheets[0].Properties == nil {
		return "", fmt.Errorf("spreadsheet has no tabs")
	}
	return ss.Sheets[0].Properties.Title, nil
}
