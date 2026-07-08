// Package sheets appends rows to the user's Google Sheet using a service
// account with the spreadsheets scope only (no Drive access).
package sheets

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type Writer struct {
	svc     *sheets.Service
	sheetID string
	tab     string
}

// NewWriter authenticates with the service-account JSON at credsPath. The
// sheet must be shared (editor) with the service account's email.
func NewWriter(ctx context.Context, credsPath, sheetID, tab string) (*Writer, error) {
	svc, err := sheets.NewService(ctx,
		option.WithCredentialsFile(credsPath),
		option.WithScopes(sheets.SpreadsheetsScope),
	)
	if err != nil {
		return nil, fmt.Errorf("sheets service: %w", err)
	}
	return &Writer{svc: svc, sheetID: sheetID, tab: tab}, nil
}

const maxAttempts = 3

// Append adds one row after the existing data on the configured tab,
// retrying transient errors with exponential backoff + jitter.
func (w *Writer) Append(ctx context.Context, row []string) error {
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
