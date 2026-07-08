// Package store persists submissions in SQLite: dedup keys, extraction
// results, and the needs-review / failed-write queues (status filters over
// one table).
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Status string

const (
	StatusWritten     Status = "written"
	StatusNeedsReview Status = "needs_review"
	StatusFailedWrite Status = "failed_write"
)

// Submission is one processed submission.
type Submission struct {
	ID           int64
	ContentHash  string
	Status       Status
	Extraction   []byte // JSON blob of llm.Result
	Flags        []byte // JSON array of user-facing flags/reasons
	InputExcerpt string
	Error        string
	CreatedAt    time.Time
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
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash  TEXT NOT NULL UNIQUE,
			status        TEXT NOT NULL,
			extraction    TEXT,
			flags         TEXT,
			input_excerpt TEXT,
			error         TEXT,
			created_at    TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(status);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
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

// FindByHash returns the prior submission with this hash, or nil.
func (s *Store) FindByHash(ctx context.Context, hash string) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, content_hash, status, extraction, flags, input_excerpt, error, created_at
		FROM submissions WHERE content_hash = ?`, hash)
	var sub Submission
	var createdAt string
	var extraction, flags, excerpt, errMsg sql.NullString
	if err := row.Scan(&sub.ID, &sub.ContentHash, &sub.Status, &extraction, &flags, &excerpt, &errMsg, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	sub.Extraction = []byte(extraction.String)
	sub.Flags = []byte(flags.String)
	sub.InputExcerpt = excerpt.String
	sub.Error = errMsg.String
	sub.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &sub, nil
}

// Insert records a processed submission. If another request with the same
// hash won the race, the insert is a no-op and the stored submission is
// returned with duplicate=true.
func (s *Store) Insert(ctx context.Context, sub *Submission) (duplicate bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO submissions (content_hash, status, extraction, flags, input_excerpt, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING`,
		sub.ContentHash, sub.Status, string(sub.Extraction), string(sub.Flags),
		sub.InputExcerpt, sub.Error, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// ListByStatus returns submissions in a given state (needs_review /
// failed_write queues), newest first.
func (s *Store) ListByStatus(ctx context.Context, status Status, limit int) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, content_hash, status, extraction, flags, input_excerpt, error, created_at
		FROM submissions WHERE status = ? ORDER BY id DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		var sub Submission
		var createdAt string
		var extraction, flags, excerpt, errMsg sql.NullString
		if err := rows.Scan(&sub.ID, &sub.ContentHash, &sub.Status, &extraction, &flags, &excerpt, &errMsg, &createdAt); err != nil {
			return nil, err
		}
		sub.Extraction = []byte(extraction.String)
		sub.Flags = []byte(flags.String)
		sub.InputExcerpt = excerpt.String
		sub.Error = errMsg.String
		sub.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, sub)
	}
	return out, rows.Err()
}
