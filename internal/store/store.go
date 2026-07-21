// Package store persists submissions in SQLite: dedup keys, extraction
// results, the full original input for the review pane, and the pending /
// failed-write queues (status filters over one table). Every submission
// belongs to a user (multi-tenant): reads and writes are scoped by user_id so
// one tenant can never see or act on another's data. The users, sessions,
// oauth_accounts, user_sheets and auth_tokens tables live in auth.go.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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
	StatusDiscarded   Status = "discarded" // soft-deleted; row retained, dedup hash freed
)

// Submission is one processed submission, owned by a user.
type Submission struct {
	ID             int64
	UserID         int64
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
	SettledAt      time.Time // when the submission reached written or discarded; zero otherwise
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
			user_id          INTEGER,
			content_hash     TEXT NOT NULL UNIQUE,
			status           TEXT NOT NULL,
			extraction       TEXT,
			flags            TEXT,
			input_excerpt    TEXT,
			input_text       TEXT,
			input_image      BLOB,
			input_image_type TEXT,
			error            TEXT,
			created_at       TEXT NOT NULL,
			settled_at       TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_submissions_status ON submissions(status);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := createAuthTables(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate auth: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate upgrades older databases in place. SQLite has no ADD COLUMN IF NOT
// EXISTS, so existing columns are checked via PRAGMA first: input columns were
// added for the review pane, user_id for multi-tenancy. The retired
// needs_review status collapses into pending — every extraction is
// user-confirmed now.
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
		"settled_at":       "TEXT",
		"user_id":          "INTEGER",
	} {
		if !have[col] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE submissions ADD COLUMN %s %s`, col, typ)); err != nil {
				return err
			}
		}
	}

	if _, err := db.Exec(`UPDATE submissions SET status = ? WHERE status = 'needs_review'`, StatusPending); err != nil {
		return err
	}

	// The per-tenant queue index is created here, after the user_id column is
	// guaranteed to exist (it may have just been added above), so upgrading an
	// old database doesn't index a missing column.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_submissions_user_status ON submissions(user_id, status)`); err != nil {
		return err
	}

	// Rows settled before settled_at existed: approximate with created_at so
	// the retention purge eventually reaches them.
	_, err = db.Exec(`
		UPDATE submissions SET settled_at = created_at
		WHERE settled_at IS NULL AND status IN (?, ?)`,
		StatusWritten, StatusDiscarded)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// ContentHash is the per-user dedup key: the owning user's id + submitted text
// + image bytes + a day bucket. Including the user id means two different users
// submitting identical content never collide on the globally-unique
// content_hash column, while an accidental double-submit by the same user on
// the same day is still deduplicated; the same lead re-submitted on a later day
// is allowed through.
func ContentHash(userID int64, text string, image []byte, submitted time.Time) string {
	h := sha256.New()
	var uid [8]byte
	binary.BigEndian.PutUint64(uid[:], uint64(userID))
	h.Write(uid[:])
	h.Write([]byte(text))
	h.Write(image)
	h.Write([]byte(submitted.UTC().Format("2006-01-02")))
	return hex.EncodeToString(h.Sum(nil))
}

const submissionCols = `id, user_id, content_hash, status, extraction, flags, input_excerpt,
	input_text, input_image, input_image_type, error, created_at, settled_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanSubmission(row scanner) (*Submission, error) {
	var sub Submission
	var createdAt string
	var userID sql.NullInt64
	var extraction, flags, excerpt, inputText, imageType, errMsg, settledAt sql.NullString
	if err := row.Scan(&sub.ID, &userID, &sub.ContentHash, &sub.Status, &extraction, &flags,
		&excerpt, &inputText, &sub.InputImage, &imageType, &errMsg, &createdAt, &settledAt); err != nil {
		return nil, err
	}
	sub.UserID = userID.Int64
	sub.Extraction = []byte(extraction.String)
	sub.Flags = []byte(flags.String)
	sub.InputExcerpt = excerpt.String
	sub.InputText = inputText.String
	sub.InputImageType = imageType.String
	sub.Error = errMsg.String
	sub.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if settledAt.Valid {
		sub.SettledAt, _ = time.Parse(time.RFC3339, settledAt.String)
	}
	return &sub, nil
}

// FindByHash returns the given user's prior submission with this hash, or nil.
// The hash already embeds the user id; the user_id predicate is defence in
// depth against a hash collision leaking another tenant's row.
func (s *Store) FindByHash(ctx context.Context, userID int64, hash string) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE content_hash = ? AND user_id = ?`, hash, userID)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// Get returns the user's submission with this id, or nil when absent. A row
// owned by another user reads as absent (nil), so callers return 404 and ids
// stay non-enumerable across tenants.
func (s *Store) Get(ctx context.Context, userID, id int64) (*Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE id = ? AND user_id = ?`, id, userID)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// Insert records a processed submission and sets sub.ID. sub.UserID must be
// set by the caller. If another request with the same hash won the race, the
// insert is a no-op and duplicate=true.
func (s *Store) Insert(ctx context.Context, sub *Submission) (duplicate bool, err error) {
	sub.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO submissions (user_id, content_hash, status, extraction, flags, input_excerpt,
			input_text, input_image, input_image_type, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING`,
		sub.UserID, sub.ContentHash, sub.Status, string(sub.Extraction), string(sub.Flags),
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
// before confirming), and error message for one of the user's submissions.
// Reaching a settled status (written / discarded) stamps settled_at once,
// which starts the retention clock for purging the original input. Updating a
// row the user does not own is "not found".
func (s *Store) Update(ctx context.Context, userID, id int64, status Status, extraction []byte, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE submissions SET status = ?, extraction = ?, error = ?,
			settled_at = CASE WHEN ? IN (?, ?) THEN COALESCE(settled_at, ?) ELSE settled_at END
		WHERE id = ? AND user_id = ?`,
		status, string(extraction), errMsg,
		status, StatusWritten, StatusDiscarded, time.Now().UTC().Format(time.RFC3339), id, userID)
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

// Discard soft-deletes one of the user's submissions: the row is retained with
// status discarded, and the content hash is rewritten to a per-row tombstone
// ("discarded:{id}:{hash}") so the UNIQUE constraint stays satisfied while the
// original hash is freed — the same content can be genuinely resubmitted the
// same day. Discarding an already-discarded submission, or one the user does
// not own, is a no-op.
func (s *Store) Discard(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE submissions
		SET status = ?, content_hash = 'discarded:' || id || ':' || content_hash,
			settled_at = COALESCE(settled_at, ?)
		WHERE id = ? AND user_id = ? AND status != ?`,
		StatusDiscarded, time.Now().UTC().Format(time.RFC3339), id, userID, StatusDiscarded)
	return err
}

// PurgeInputs nulls the stored original input (full text, image blob, image
// type) of submissions settled before the cutoff. It runs across all tenants
// as a background retention sweep. Extraction results and the short excerpt are
// kept — only raw originals are purged. Returns the number of rows purged.
func (s *Store) PurgeInputs(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE submissions
		SET input_text = NULL, input_image = NULL, input_image_type = NULL
		WHERE status IN (?, ?)
		  AND settled_at IS NOT NULL AND settled_at < ?
		  AND (input_text IS NOT NULL OR input_image IS NOT NULL)`,
		StatusWritten, StatusDiscarded, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListByStatus returns the user's submissions in a given state, newest first.
func (s *Store) ListByStatus(ctx context.Context, userID int64, status Status, limit int) ([]Submission, error) {
	return s.ListByStatuses(ctx, userID, []Status{status}, limit)
}

// ListByStatuses returns the user's submissions in any of the given states,
// newest first.
func (s *Store) ListByStatuses(ctx context.Context, userID int64, statuses []Status, limit int) ([]Submission, error) {
	placeholders, args := statusArgs(statuses)
	args = append([]any{userID}, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+submissionCols+`
		FROM submissions WHERE user_id = ? AND status IN (`+placeholders+`) ORDER BY id DESC LIMIT ?`, args...)
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

// CountByStatus counts the user's submissions in any of the given states
// (queue badge).
func (s *Store) CountByStatus(ctx context.Context, userID int64, statuses ...Status) (int, error) {
	placeholders, args := statusArgs(statuses)
	args = append([]any{userID}, args...)
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions WHERE user_id = ? AND status IN (`+placeholders+`)`, args...).Scan(&n)
	return n, err
}

func statusArgs(statuses []Status) (string, []any) {
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = st
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ","), args
}
