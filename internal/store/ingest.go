package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventStatus is the state of an ingestion event — a piece of mail that
// arrived for a user and did not (yet) become a submission.
type EventStatus string

const (
	// EventQuarantined is mail the filter pipeline rejected. It is never
	// dropped: the user can see why and rescue it.
	EventQuarantined EventStatus = "quarantined"
	// EventRescued is a quarantined item the user pushed into the review queue.
	EventRescued EventStatus = "rescued"
	// EventDismissed is a quarantined item the user acknowledged and closed.
	EventDismissed EventStatus = "dismissed"
	// EventVerification is a provider forwarding-confirmation handshake (a
	// code and/or link the user must act on), not a lead at all.
	EventVerification EventStatus = "verification"
)

// InboundAddress is a user's private capture address.
//
// CFRuleID is the Cloudflare Email Routing rule that makes the address
// deliverable. Cloudflare does not support catch-all on a subdomain, so each
// address costs one rule; without a rule, mail to this address is rejected at
// SMTP and never reaches us. An address with no CFRuleID is provisioned only
// halfway and must not be shown to the user as working.
type InboundAddress struct {
	ID            int64
	UserID        int64
	LocalPart     string
	Active        bool
	CFRuleID      string
	ProvisionedAt time.Time
	CreatedAt     time.Time
	RetiredAt     time.Time
}

// Provisioned reports whether the address is actually deliverable.
func (a *InboundAddress) Provisioned() bool { return a.CFRuleID != "" }

// IngestionEvent is one inbound mail that did not become a submission.
type IngestionEvent struct {
	ID           int64
	UserID       int64
	Status       EventStatus
	Reason       string
	Detail       string
	MessageID    string
	FromAddress  string
	FromName     string
	Subject      string
	ReceivedAt   time.Time
	BodyExcerpt  string
	BodyText     string
	Provenance   []byte
	VerifyCode   string
	VerifyURL    string
	SubmissionID int64
	CreatedAt    time.Time
	SettledAt    time.Time
}

// createIngestTables creates the email-ingestion tables. Called from Open on
// every boot, like createAuthTables and createDestinationTable.
func createIngestTables(db *sql.DB) error {
	_, err := db.Exec(`
	-- Not keyed on user_id alone: rotation keeps the previous address present
	-- but inactive for a grace period, because the user's forwarding rule still
	-- points at it and mail is already in flight.
	CREATE TABLE IF NOT EXISTS inbound_addresses (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id        INTEGER NOT NULL,
		local_part     TEXT NOT NULL,
		active         INTEGER NOT NULL DEFAULT 1,
		cf_rule_id     TEXT,
		provisioned_at TEXT,
		created_at     TEXT NOT NULL,
		retired_at     TEXT
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_inbound_local_part ON inbound_addresses(local_part);
	CREATE INDEX IF NOT EXISTS idx_inbound_user ON inbound_addresses(user_id, active);

	-- Everything that arrived by email and did not become a submission.
	-- body_text is the cleaned text a rescue re-extracts from and is nulled by
	-- the retention purge; body_excerpt is short and survives it, so the
	-- quarantine list stays readable after a purge.
	CREATE TABLE IF NOT EXISTS ingestion_events (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id       INTEGER NOT NULL,
		status        TEXT NOT NULL,
		reason        TEXT NOT NULL,
		detail        TEXT,
		message_id    TEXT,
		from_address  TEXT,
		from_name     TEXT,
		subject       TEXT,
		received_at   TEXT NOT NULL,
		body_excerpt  TEXT NOT NULL,
		body_text     TEXT,
		provenance    TEXT,
		verify_code   TEXT,
		verify_url    TEXT,
		submission_id INTEGER,
		created_at    TEXT NOT NULL,
		settled_at    TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_ingestion_events_user_status
		ON ingestion_events(user_id, status, id DESC);

	-- pattern is lowercased and is either a full address ("spam@x.com") or a
	-- domain suffix ("@x.com").
	CREATE TABLE IF NOT EXISTS blocked_senders (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL,
		pattern    TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_blocked_senders_user_pattern
		ON blocked_senders(user_id, pattern);
	`)
	return err
}

const inboundCols = `id, user_id, local_part, active, cf_rule_id, provisioned_at, created_at, retired_at`

func scanInbound(row scanner) (*InboundAddress, error) {
	var a InboundAddress
	var active int
	var createdAt string
	var ruleID, provisionedAt, retiredAt sql.NullString
	if err := row.Scan(&a.ID, &a.UserID, &a.LocalPart, &active, &ruleID,
		&provisionedAt, &createdAt, &retiredAt); err != nil {
		return nil, err
	}
	a.Active = active == 1
	a.CFRuleID = ruleID.String
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if provisionedAt.Valid {
		a.ProvisionedAt, _ = time.Parse(time.RFC3339, provisionedAt.String)
	}
	if retiredAt.Valid {
		a.RetiredAt, _ = time.Parse(time.RFC3339, retiredAt.String)
	}
	return &a, nil
}

// CreateInboundAddress records a new active address for the user with no
// routing rule yet. The caller provisions the rule and then calls
// SetInboundRuleID; until it does, the address is not deliverable.
func (s *Store) CreateInboundAddress(ctx context.Context, userID int64, localPart string) (*InboundAddress, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO inbound_addresses (user_id, local_part, active, created_at)
		VALUES (?, ?, 1, ?)`, userID, localPart, now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &InboundAddress{ID: id, UserID: userID, LocalPart: localPart, Active: true, CreatedAt: now}, nil
}

// SetInboundRuleID marks an address deliverable by recording the routing rule
// that carries its mail.
func (s *Store) SetInboundRuleID(ctx context.Context, userID, id int64, ruleID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE inbound_addresses SET cf_rule_id = ?, provisioned_at = ?
		WHERE id = ? AND user_id = ?`,
		ruleID, time.Now().UTC().Format(time.RFC3339), id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("inbound address %d not found", id)
	}
	return nil
}

// ActiveInboundAddress returns the user's current address, or nil when they
// have not enabled email capture.
func (s *Store) ActiveInboundAddress(ctx context.Context, userID int64) (*InboundAddress, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+inboundCols+` FROM inbound_addresses
		WHERE user_id = ? AND active = 1 ORDER BY id DESC LIMIT 1`, userID)
	a, err := scanInbound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// LookupInboundAddress resolves a local part to its owner, or nil when no such
// address exists. Retired addresses still resolve so mail already in flight is
// attributable — the caller checks Active and decides what to do.
func (s *Store) LookupInboundAddress(ctx context.Context, localPart string) (*InboundAddress, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+inboundCols+` FROM inbound_addresses WHERE local_part = ?`, localPart)
	a, err := scanInbound(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// RetireInboundAddresses marks every current address of the user inactive.
// Rotation calls this after the replacement is live.
func (s *Store) RetireInboundAddresses(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE inbound_addresses SET active = 0, retired_at = COALESCE(retired_at, ?)
		WHERE user_id = ? AND active = 1`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// DeleteInboundAddress removes an address row outright. Used once its routing
// rule has been deleted, so the two never drift apart.
func (s *Store) DeleteInboundAddress(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM inbound_addresses WHERE id = ?`, id)
	return err
}

// CountActiveInboundAddresses counts addresses across all tenants. Routing
// rules are a globally scarce resource (Cloudflare caps them per domain), so
// this is deliberately not user-scoped: it is the budget check.
func (s *Store) CountActiveInboundAddresses(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inbound_addresses WHERE active = 1`).Scan(&n)
	return n, err
}

// RetiredInboundAddressesBefore returns retired addresses whose grace period
// has elapsed, so their routing rules can be released.
func (s *Store) RetiredInboundAddressesBefore(ctx context.Context, cutoff time.Time) ([]InboundAddress, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+inboundCols+` FROM inbound_addresses
		WHERE active = 0 AND retired_at IS NOT NULL AND retired_at < ?`,
		cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboundAddress
	for rows.Next() {
		a, err := scanInbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

const eventCols = `id, user_id, status, reason, detail, message_id, from_address, from_name,
	subject, received_at, body_excerpt, body_text, provenance, verify_code, verify_url,
	submission_id, created_at, settled_at`

func scanEvent(row scanner) (*IngestionEvent, error) {
	var e IngestionEvent
	var receivedAt, createdAt string
	var detail, messageID, fromAddress, fromName, subject sql.NullString
	var bodyText, provenance, verifyCode, verifyURL, settledAt sql.NullString
	var submissionID sql.NullInt64
	if err := row.Scan(&e.ID, &e.UserID, &e.Status, &e.Reason, &detail, &messageID,
		&fromAddress, &fromName, &subject, &receivedAt, &e.BodyExcerpt, &bodyText,
		&provenance, &verifyCode, &verifyURL, &submissionID, &createdAt, &settledAt); err != nil {
		return nil, err
	}
	e.Detail = detail.String
	e.MessageID = messageID.String
	e.FromAddress = fromAddress.String
	e.FromName = fromName.String
	e.Subject = subject.String
	e.BodyText = bodyText.String
	e.Provenance = []byte(provenance.String)
	e.VerifyCode = verifyCode.String
	e.VerifyURL = verifyURL.String
	e.SubmissionID = submissionID.Int64
	e.ReceivedAt, _ = time.Parse(time.RFC3339, receivedAt)
	e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if settledAt.Valid {
		e.SettledAt, _ = time.Parse(time.RFC3339, settledAt.String)
	}
	return &e, nil
}

// InsertEvent records one piece of mail that did not become a submission, and
// sets ev.ID. ev.UserID must be set by the caller: an event always belongs to
// exactly one tenant, which is why mail to an unknown address is counted and
// logged rather than stored — it has no owner to show it to.
func (s *Store) InsertEvent(ctx context.Context, ev *IngestionEvent) error {
	ev.CreatedAt = time.Now().UTC()
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = ev.CreatedAt
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO ingestion_events (user_id, status, reason, detail, message_id, from_address,
			from_name, subject, received_at, body_excerpt, body_text, provenance,
			verify_code, verify_url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.UserID, ev.Status, ev.Reason, ev.Detail, ev.MessageID, ev.FromAddress,
		ev.FromName, ev.Subject, ev.ReceivedAt.UTC().Format(time.RFC3339),
		ev.BodyExcerpt, ev.BodyText, string(ev.Provenance), ev.VerifyCode, ev.VerifyURL,
		ev.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return err
	}
	ev.ID, err = res.LastInsertId()
	return err
}

// ListEvents returns the user's ingestion events in the given states, newest
// first.
func (s *Store) ListEvents(ctx context.Context, userID int64, statuses []EventStatus, limit int) ([]IngestionEvent, error) {
	args := []any{userID}
	for _, st := range statuses {
		args = append(args, st)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+eventCols+` FROM ingestion_events
		WHERE user_id = ? AND status IN (`+placeholders+`)
		ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IngestionEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// GetEvent returns the user's event with this id, or nil when absent. An event
// owned by another user reads as absent, so callers 404 and ids stay
// non-enumerable across tenants — the same rule as Get for submissions.
func (s *Store) GetEvent(ctx context.Context, userID, id int64) (*IngestionEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+eventCols+` FROM ingestion_events WHERE id = ? AND user_id = ?`, id, userID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// SettleEvent moves an event to a terminal state (rescued or dismissed),
// stamping settled_at once so the retention clock starts. submissionID is the
// row a rescue created, or 0. Settling an event the user does not own, or one
// already settled, is "not found".
func (s *Store) SettleEvent(ctx context.Context, userID, id int64, status EventStatus, submissionID int64) error {
	var subID any
	if submissionID != 0 {
		subID = submissionID
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE ingestion_events
		SET status = ?, submission_id = ?, settled_at = COALESCE(settled_at, ?)
		WHERE id = ? AND user_id = ? AND status IN (?, ?)`,
		status, subID, time.Now().UTC().Format(time.RFC3339), id, userID,
		EventQuarantined, EventVerification)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("ingestion event %d not found", id)
	}
	return nil
}

// CountEvents counts the user's events in the given states (quarantine badge).
func (s *Store) CountEvents(ctx context.Context, userID int64, statuses ...EventStatus) (int, error) {
	args := []any{userID}
	for _, st := range statuses {
		args = append(args, st)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ingestion_events
		WHERE user_id = ? AND status IN (`+placeholders+`)`, args...).Scan(&n)
	return n, err
}

// PurgeIngestionBodies nulls the stored body of settled events past the
// cutoff, across all tenants. Quarantine holds full text of mail the user
// never asked to receive, so it gets the same retention clock as submission
// inputs. The short excerpt is kept so the list stays readable.
func (s *Store) PurgeIngestionBodies(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE ingestion_events SET body_text = NULL
		WHERE settled_at IS NOT NULL AND settled_at < ? AND body_text IS NOT NULL`,
		cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BlockedSenders returns the user's blocklist patterns, lowercased.
func (s *Store) BlockedSenders(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pattern FROM blocked_senders WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BlockSender adds a pattern to the user's blocklist. Blocking the same
// pattern twice is a no-op.
func (s *Store) BlockSender(ctx context.Context, userID int64, pattern string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blocked_senders (user_id, pattern, created_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id, pattern) DO NOTHING`,
		userID, strings.ToLower(strings.TrimSpace(pattern)), time.Now().UTC().Format(time.RFC3339))
	return err
}

// UnblockSender removes one of the user's blocklist patterns.
func (s *Store) UnblockSender(ctx context.Context, userID int64, pattern string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM blocked_senders WHERE user_id = ? AND pattern = ?`,
		userID, strings.ToLower(strings.TrimSpace(pattern)))
	return err
}

// EmailsToday counts the user's email-sourced submissions created since the
// given instant. This is the persistent half of the ingestion cap: an
// in-memory limiter resets when the process restarts, this does not, so a
// restart loop cannot be used to refill the budget.
func (s *Store) EmailsToday(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions
		WHERE user_id = ? AND source = ? AND created_at >= ?`,
		userID, SourceEmail, since.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// FindByMessageID returns the user's prior submission for this Message-ID
// since the given instant, or nil.
//
// This is the dedup that catches a mail system redelivering the same message:
// the content hash buckets by calendar day, so a retry that crosses midnight
// UTC hashes differently and would otherwise insert a second copy.
//
// Discarded submissions are excluded, mirroring how Discard tombstones the
// content hash. Without that, discarding an email lead would silently block
// the sender from ever being captured again on that message — the hash would
// be freed but the Message-ID would go on matching forever, and the second
// forward would vanish with no trace for the user to find.
func (s *Store) FindByMessageID(ctx context.Context, userID int64, messageID string, since time.Time) (*Submission, error) {
	if messageID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT `+submissionCols+` FROM submissions
		WHERE user_id = ? AND message_id = ? AND created_at >= ? AND status != ?
		ORDER BY id DESC LIMIT 1`,
		userID, messageID, since.UTC().Format(time.RFC3339), StatusDiscarded)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

// MarkQueueSeen records that the user has looked at their review queue, which
// is what "captured while you were away" counts forward from.
func (s *Store) MarkQueueSeen(ctx context.Context, userID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET queue_seen_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), userID)
	return err
}

// QueueSeenAt returns when the user last looked at their review queue, or the
// zero time if never.
func (s *Store) QueueSeenAt(ctx context.Context, userID int64) (time.Time, error) {
	var seen sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT queue_seen_at FROM users WHERE id = ?`, userID).Scan(&seen)
	if err != nil || !seen.Valid {
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, seen.String)
	return t, nil
}

// CountCapturedSince counts email-sourced submissions still awaiting review
// that arrived after the given instant — the "while you were away" number.
func (s *Store) CountCapturedSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions
		WHERE user_id = ? AND source = ? AND status = ? AND created_at > ?`,
		userID, SourceEmail, StatusPending, since.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}
