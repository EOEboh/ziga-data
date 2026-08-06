// Package sheets appends rows to the user's Google Sheet using a service
// account with the spreadsheets scope only (no Drive access).
package sheets

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/EOEboh/ziga-data/internal/destination"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type Writer struct {
	svc     *sheets.Service
	sheetID string
	tab     string
	// header holds the schema column names when row 1 of the tab is a
	// header (HEADER_ROW mode); nil means the tab has no header row. In
	// header mode, the first append into a completely empty tab writes the
	// header row first.
	header        []string
	headerMu      sync.Mutex
	headerChecked bool
}

// NewWriter authenticates with the service-account JSON at credsPath. The
// sheet must be shared (editor) with the service account's email. header is
// the column-name row to maintain on the tab, or nil for no-header mode.
func NewWriter(ctx context.Context, credsPath, sheetID, tab string, header []string) (*Writer, error) {
	svc, err := sheets.NewService(ctx,
		option.WithCredentialsFile(credsPath),
		option.WithScopes(sheets.SpreadsheetsScope),
	)
	if err != nil {
		return nil, fmt.Errorf("sheets service: %w", err)
	}
	return &Writer{svc: svc, sheetID: sheetID, tab: tab, header: header}, nil
}

const maxAttempts = 3

// Write appends one lead as a row after the existing data on the configured
// tab, implementing destination.Writer. In header mode, an empty tab gets the
// header row written first.
//
// A sheet has a column for every schema field by construction, so no field is
// ever dropped and the returned Result is always empty.
func (w *Writer) Write(ctx context.Context, lead destination.Lead) (destination.Result, error) {
	if err := w.ensureHeader(ctx); err != nil {
		return destination.Result{}, err
	}
	return destination.Result{}, w.appendRow(ctx, lead.Values())
}

// ensureHeader checks the tab once per process; a transient failure leaves
// headerChecked unset so the next confirm retries the check.
func (w *Writer) ensureHeader(ctx context.Context) error {
	if w.header == nil {
		return nil
	}
	w.headerMu.Lock()
	defer w.headerMu.Unlock()
	if w.headerChecked {
		return nil
	}
	resp, err := w.svc.Spreadsheets.Values.Get(w.sheetID, w.tab).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("sheets header check: %w", err)
	}
	if len(resp.Values) == 0 {
		if err := w.appendRow(ctx, w.header); err != nil {
			return err
		}
	}
	w.headerChecked = true
	return nil
}

// appendRow performs the raw append, retrying transient errors with
// exponential backoff + jitter.
func (w *Writer) appendRow(ctx context.Context, row []string) error {
	vals := make([]any, len(row))
	for i, v := range row {
		vals[i] = v
	}
	body := &sheets.ValueRange{Values: [][]any{vals}}

	backoff := time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := backoff + time.Duration(rand.Int64N(int64(backoff/2)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			backoff *= 2
		}
		_, err := w.svc.Spreadsheets.Values.Append(w.sheetID, w.tab, body).
			ValueInputOption("RAW").
			InsertDataOption("INSERT_ROWS").
			Context(ctx).
			Do()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			return fmt.Errorf("sheets append: %w", err)
		}
	}
	return fmt.Errorf("sheets append failed after %d attempts: %w", maxAttempts, lastErr)
}

// retryable: rate limits, server errors, and network failures are worth
// retrying; auth/permission/not-found errors are not.
func retryable(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 429 || gerr.Code >= 500
	}
	return true // transport-level error
}
