// Package store persists submissions in SQLite: dedup keys, extraction
// results, the full original input for the review pane, and the pending /
// failed-write queues (status filters over one table).
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Status string

const (
	StatusPending     Status = "pending" // extracted, awaiting user confirm
	StatusWritten     Status = "written"
	StatusFailedWrite Status = "failed_write"
)

// Submission is one processed submission.
type Submission struct {
	ID             int64
	ContentHash    string
	Status         Status
	Extraction     []byte // JSON blob of llm.Result
	Flags          []byte // JSON array of user-facing flags
	InputExcerpt   string
	InputText      string // full submitted text, for the review pane
	InputImage     []byte // submitted image bytes, if any
	InputImageType string // e.g. "image/png"
	Error          string
	CreatedAt      time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite handles one writer at a time; keep the pool at one connection to
	// avoid SQLITE_BUSY under concurrent submissions.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS submissions (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash     TEXT NOT NULL UNIQUE,
			status           TEXT NOT NULL,
			extraction       TEXT,
			flags            TEXT,
			input_excerpt    TEXT,
			input_text       TEXT,
			input_image      BLOB,
			input_image_type TEXT,
			error            TEXT,
			created_at       TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(status);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate upgrades databases created before the review-pane rework: input
// columns are added (SQLite has no ADD COLUMN IF NOT EXISTS, so existing
// columns are checked via PRAGMA first) and the retired needs_review status
// collapses into pending — every extraction is user-confirmed now.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(submissions)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for col, typ := range map[string]string{
		"input_text":       "TEXT",
		"input_image":      "BLOB",
		"input_image_type": "TEXT",
	} {
		if !have[col] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE submissions ADD COLUMN %s %s`, col, typ)); err != nil {
				return err
			}
		}
	}

	_, err = db.Exec(`UPDATE submissions SET status = ? WHERE status = 'needs_review'`, StatusPending)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ContentHash is the dedup key: submitted text + image bytes + a day bucket,
// so an accidental double-submit doesn't create a duplicate row, but the same
// lead re-submitted on a later day is allowed through.
func ContentHash(text string, image []byte, submitted time.Time) string {
	h := sha256.New()
	h.Write([]byte(text))
	h.Write(image)
	h.Write([]byte(submitted.UTC().Format("2006-01-02")))
	return hex.EncodeToString(h.Sum(nil))
}

const submissionCols = `id, content_hash, status, extraction, flags, input_excerpt,
	input_text, input_image, input_image_type, error, created_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanSubmission(row scanner) (*Submission, error) {
	var sub Submission
	var createdAt string
	var extraction, flags, excerpt, inputText, imageType, errMsg sql.NullString
	if err := row.Scan(&sub.ID, &sub.ContentHash, &sub.Status, &extraction, &flags,
		&excerpt, &inputText, &sub.InputImage, &imageType, &errMsg, &createdAt); err != nil {
		return nil, err
	}
	sub.Extraction = []byte(extraction.String)
	sub.Flags = []byte(flags.String)
	sub.InputExcerpt = excerpt.String
	sub.InputText = inputText.String
	sub.InputImageType = imageType.String
	sub.Error = errMsg.String
	sub.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &sub, nil
}

// FindByHash returns the prior submission with this hash, or nil.
func (s *Store) FindByHash(ctx context.Context, hash string) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE content_hash = ?`, hash)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// Get returns the submission with this id, or nil when absent.
func (s *Store) Get(ctx context.Context, id int64) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE id = ?`, id)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// Insert records a processed submission and sets sub.ID. If another request
// with the same hash won the race, the insert is a no-op and duplicate=true.
func (s *Store) Insert(ctx context.Context, sub *Submission) (duplicate bool, err error) {
	sub.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO submissions (content_hash, status, extraction, flags, input_excerpt,
			input_text, input_image, input_image_type, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING`,
		sub.ContentHash, sub.Status, string(sub.Extraction), string(sub.Flags),
		sub.InputExcerpt, sub.InputText, sub.InputImage, sub.InputImageType,
		sub.Error, sub.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return true, nil
	}
	sub.ID, err = res.LastInsertId()
	return false, err
}

// Update sets the status, extraction blob (the user may have edited fields
// before confirming), and error message for one submission.
func (s *Store) Update(ctx context.Context, id int64, status Status, extraction []byte, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE submissions SET status = ?, extraction = ?, error = ? WHERE id = ?`,
		status, string(extraction), errMsg, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("submission %d not found", id)
	}
	return nil
}

// Delete removes a submission (discard). Hard delete: it frees the content
// hash so the same content can be genuinely resubmitted the same day.
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM submissions WHERE id = ?`, id)
	return err
}

// ListByStatus returns submissions in a given state, newest first.
func (s *Store) ListByStatus(ctx context.Context, status Status, limit int) ([]Submission, error) {
	return s.ListByStatuses(ctx, []Status{status}, limit)
}

// ListByStatuses returns submissions in any of the given states, newest first.
func (s *Store) ListByStatuses(ctx context.Context, statuses []Status, limit int) ([]Submission, error) {
	placeholders, args := statusArgs(statuses)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE status IN (`+placeholders+`) ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// CountByStatus counts submissions in any of the given states (queue badge).
func (s *Store) CountByStatus(ctx context.Context, statuses ...Status) (int, error) {
	placeholders, args := statusArgs(statuses)
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions WHERE status IN (`+placeholders+`)`, args...).Scan(&n)
	return n, err
}

func statusArgs(statuses []Status) (string, []any) {
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = st
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ","), args
}
