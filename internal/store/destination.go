package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Destination is a user's single lead destination, generalized over
// destination types. Config is a per-type JSON blob (SheetConfig or
// NotionConfig below); this package stores it opaquely so adding a third
// destination type needs no schema change. BrokenAt marks a destination whose
// access was lost — the UI prompts a reconnect rather than failing writes
// silently.
type Destination struct {
	UserID      int64
	Type        string
	Config      []byte
	ConnectedAt time.Time
	BrokenAt    time.Time
}

func (d *Destination) Broken() bool { return !d.BrokenAt.IsZero() }

// SheetConfig is the Config payload for type google_sheet. Its field names
// match the columns of the legacy user_sheets table, which is what the
// backfill below writes.
type SheetConfig struct {
	SpreadsheetID string `json:"spreadsheet_id"`
	SheetTab      string `json:"sheet_tab"`
	CreatedByApp  bool   `json:"created_by_app"`
}

// NotionConfig is the Config payload for type notion.
//
// Both DatabaseID and DataSourceID are stored. Since Notion API version
// 2025-09-03 a database is a parent of one or more data sources and the
// property schema lives on the data source, so writes and queries address the
// data source while the database id remains the user-facing identity (it is
// what the workspace UI shows and what a page URL is built from).
//
// Mapping keys are Ziga schema field names; values name a Notion property
// exactly as the API returned it — Notion property names are case-sensitive,
// so "Name" and "name" are different properties and the casing must round-trip
// untouched.
type NotionConfig struct {
	WorkspaceID   string                    `json:"workspace_id"`
	WorkspaceName string                    `json:"workspace_name"`
	DatabaseID    string                    `json:"database_id"`
	DataSourceID  string                    `json:"data_source_id"`
	DatabaseTitle string                    `json:"database_title"`
	CreatedByApp  bool                      `json:"created_by_app"`
	Mapping       map[string]MappedProperty `json:"mapping"`
}

// MappedProperty is one Ziga field's target Notion property: its exact name
// and its Notion type, so the writer can coerce the value without re-fetching
// the schema on every write.
type MappedProperty struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SheetConfig decodes the config as a Google Sheets destination.
func (d *Destination) SheetConfig() (*SheetConfig, error) {
	var c SheetConfig
	if err := json.Unmarshal(d.Config, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// NotionConfig decodes the config as a Notion destination.
func (d *Destination) NotionConfig() (*NotionConfig, error) {
	var c NotionConfig
	if err := json.Unmarshal(d.Config, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// createDestinationTable creates the generalized destination table. Called
// from createAuthTables on every boot.
func createDestinationTable(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS destinations (
			user_id      INTEGER PRIMARY KEY,
			type         TEXT NOT NULL,
			config       TEXT NOT NULL,
			connected_at TEXT NOT NULL,
			broken_at    TEXT
		);
	`)
	return err
}

// backfillDestinations migrates pre-existing Google Sheets users into the
// generalized model. Every user_sheets row without a destinations row becomes
// a google_sheet destination carrying the same spreadsheet, tab, and
// created-by-app flag, preserving connected_at and broken_at — so an existing
// user keeps writing to the same sheet with no reconnect and no data loss.
//
// The legacy user_sheets table is deliberately left in place and unread after
// this: rolling back to the previous binary is then just a binary swap. The
// INSERT is a no-op on rows already migrated, so this is safe on every boot.
func backfillDestinations(db *sql.DB) error {
	// Guard on the legacy table existing at all: a database created fresh by
	// this build still has it (createAuthTables makes it), but being explicit
	// keeps this safe if it is ever dropped.
	var legacy string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'user_sheets'`).Scan(&legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The config JSON is built in Go rather than SQL so SheetConfig stays the
	// single definition of the payload shape.
	rows, err := db.Query(`
		SELECT s.user_id, s.spreadsheet_id, s.sheet_tab, s.created_by_app, s.connected_at, s.broken_at
		FROM user_sheets s
		WHERE NOT EXISTS (SELECT 1 FROM destinations d WHERE d.user_id = s.user_id)`)
	if err != nil {
		return err
	}
	type legacyRow struct {
		userID      int64
		config      []byte
		connectedAt string
		brokenAt    sql.NullString
	}
	var pending []legacyRow
	for rows.Next() {
		var (
			userID       int64
			cfg          SheetConfig
			createdByApp int
			connectedAt  string
			brokenAt     sql.NullString
		)
		if err := rows.Scan(&userID, &cfg.SpreadsheetID, &cfg.SheetTab, &createdByApp,
			&connectedAt, &brokenAt); err != nil {
			rows.Close()
			return err
		}
		cfg.CreatedByApp = createdByApp != 0
		blob, err := json.Marshal(cfg)
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, legacyRow{userID, blob, connectedAt, brokenAt})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range pending {
		var broken any
		if r.brokenAt.Valid && r.brokenAt.String != "" {
			broken = r.brokenAt.String
		}
		if _, err := db.Exec(`
			INSERT INTO destinations (user_id, type, config, connected_at, broken_at)
			VALUES (?, ?, ?, ?, ?)`,
			r.userID, string(destinationTypeGoogleSheet), string(r.config), r.connectedAt, broken); err != nil {
			return err
		}
	}
	return nil
}

// destinationTypeGoogleSheet mirrors destination.TypeGoogleSheet. The store
// cannot import the destination package (the dependency runs the other way),
// so the one string it needs for the backfill is spelled out here; the
// destination package's constant is the reference definition.
const destinationTypeGoogleSheet = "google_sheet"

// SetDestination sets (or replaces) the user's single destination, clearing any
// broken state. Replacing is how a user switches between Google Sheets and
// Notion: exactly one destination is active at a time.
func (s *Store) SetDestination(ctx context.Context, d *Destination) error {
	if d.ConnectedAt.IsZero() {
		d.ConnectedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO destinations (user_id, type, config, connected_at, broken_at)
		VALUES (?, ?, ?, ?, NULL)
		ON CONFLICT(user_id) DO UPDATE SET
			type         = excluded.type,
			config       = excluded.config,
			connected_at = excluded.connected_at,
			broken_at    = NULL`,
		d.UserID, d.Type, string(d.Config), d.ConnectedAt.UTC().Format(time.RFC3339))
	return err
}

// GetDestination returns the user's destination, or ErrNotFound.
func (s *Store) GetDestination(ctx context.Context, userID int64) (*Destination, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, type, config, connected_at, broken_at
		FROM destinations WHERE user_id = ?`, userID)
	var d Destination
	var config, connectedAt string
	var brokenAt sql.NullString
	if err := row.Scan(&d.UserID, &d.Type, &config, &connectedAt, &brokenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.Config = []byte(config)
	d.ConnectedAt, _ = time.Parse(time.RFC3339, connectedAt)
	if brokenAt.Valid && brokenAt.String != "" {
		d.BrokenAt, _ = time.Parse(time.RFC3339, brokenAt.String)
	}
	return &d, nil
}

// MarkDestinationBroken flags the destination as unwritable (revoked access).
// Idempotent: it keeps the first timestamp.
func (s *Store) MarkDestinationBroken(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE destinations SET broken_at = COALESCE(broken_at, ?) WHERE user_id = ?`,
		time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// DeleteDestination removes the user's destination (disconnect).
func (s *Store) DeleteDestination(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM destinations WHERE user_id = ?`, userID)
	return err
}
