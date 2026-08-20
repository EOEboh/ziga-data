package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDestinationRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "dest@example.com", "")

	if _, err := st.GetDestination(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no destination yet should be ErrNotFound, got %v", err)
	}

	blob, _ := json.Marshal(SheetConfig{SpreadsheetID: "sheet-1", SheetTab: "Leads", CreatedByApp: true})
	if err := st.SetDestination(ctx, &Destination{
		UserID: u.ID, Type: "google_sheet", Config: blob,
	}); err != nil {
		t.Fatal(err)
	}
	dest, err := st.GetDestination(ctx, u.ID)
	if err != nil || dest.Type != "google_sheet" || dest.Broken() {
		t.Fatalf("get destination: %+v err=%v", dest, err)
	}
	cfg, err := dest.SheetConfig()
	if err != nil || cfg.SpreadsheetID != "sheet-1" || !cfg.CreatedByApp {
		t.Fatalf("sheet config: %+v err=%v", cfg, err)
	}

	if err := st.MarkDestinationBroken(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if dest, _ = st.GetDestination(ctx, u.ID); !dest.Broken() {
		t.Fatal("destination should be broken")
	}

	// Switching destination type replaces the row and clears broken — a user
	// has exactly one active destination at a time.
	notion, _ := json.Marshal(NotionConfig{
		WorkspaceID: "ws-1", DatabaseID: "db-1", DataSourceID: "ds-1",
		Mapping: map[string]MappedProperty{"name": {Name: "Name", Type: "title"}},
	})
	if err := st.SetDestination(ctx, &Destination{UserID: u.ID, Type: "notion", Config: notion}); err != nil {
		t.Fatal(err)
	}
	dest, _ = st.GetDestination(ctx, u.ID)
	if dest.Type != "notion" || dest.Broken() {
		t.Fatalf("switch destination not applied: %+v", dest)
	}
	ncfg, err := dest.NotionConfig()
	if err != nil || ncfg.DataSourceID != "ds-1" || ncfg.Mapping["name"].Name != "Name" {
		t.Fatalf("notion config: %+v err=%v", ncfg, err)
	}

	if err := st.DeleteDestination(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDestination(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted destination should be ErrNotFound, got %v", err)
	}
}

// TestBackfillMigratesExistingSheetUsers is the migration guarantee for users
// who connected a Google Sheet before the destination model was generalized:
// opening their database with this build must produce an equivalent
// destination, with no reconnect and nothing lost. The legacy rows are written
// with raw SQL because that is exactly what the previous binary left behind —
// the Go accessors for user_sheets no longer exist.
func TestBackfillMigratesExistingSheetUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	connected := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	broken := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	// A database as the previous binary left it: user_sheets rows, no
	// destinations table.
	func() {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.db.Exec(`DROP TABLE destinations`); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			uid          int64
			sheetID, tab string
			createdByApp int
			brokenAt     any
		}{
			{1, "sheet-app", "Leads", 1, nil},
			{2, "sheet-picked", "Contacts", 0, nil},
			{3, "sheet-broken", "Leads", 1, broken},
		} {
			if _, err := st.db.Exec(`
				INSERT INTO user_sheets (user_id, spreadsheet_id, sheet_tab, created_by_app, connected_at, broken_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				row.uid, row.sheetID, row.tab, row.createdByApp, connected, row.brokenAt); err != nil {
				t.Fatal(err)
			}
		}
	}()

	// Reopening runs the migration.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen legacy database: %v", err)
	}
	defer st.Close()

	for _, want := range []struct {
		uid          int64
		sheetID, tab string
		createdByApp bool
		broken       bool
	}{
		{1, "sheet-app", "Leads", true, false},
		{2, "sheet-picked", "Contacts", false, false},
		{3, "sheet-broken", "Leads", true, true},
	} {
		dest, err := st.GetDestination(ctx, want.uid)
		if err != nil {
			t.Fatalf("user %d has no destination after migration: %v", want.uid, err)
		}
		if dest.Type != destinationTypeGoogleSheet {
			t.Fatalf("user %d type = %q, want google_sheet", want.uid, dest.Type)
		}
		cfg, err := dest.SheetConfig()
		if err != nil {
			t.Fatalf("user %d config: %v", want.uid, err)
		}
		if cfg.SpreadsheetID != want.sheetID || cfg.SheetTab != want.tab || cfg.CreatedByApp != want.createdByApp {
			t.Fatalf("user %d config = %+v, want %s/%s/%v",
				want.uid, cfg, want.sheetID, want.tab, want.createdByApp)
		}
		// A destination that was broken before the migration stays broken (the
		// user still needs to reconnect); a healthy one stays healthy and must
		// not be forced through a reconnect it never needed.
		if dest.Broken() != want.broken {
			t.Fatalf("user %d broken = %v, want %v", want.uid, dest.Broken(), want.broken)
		}
		if dest.ConnectedAt.Format(time.RFC3339) != connected {
			t.Fatalf("user %d connected_at = %s, want %s", want.uid, dest.ConnectedAt, connected)
		}
	}

	// The legacy table is retained (rollback stays a binary swap).
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM user_sheets`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("user_sheets should be retained with 3 rows, got %d err=%v", n, err)
	}
}

// The backfill runs on every boot, so it must never duplicate or clobber.
func TestBackfillIsIdempotentAndDoesNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO user_sheets (user_id, spreadsheet_id, sheet_tab, created_by_app, connected_at, broken_at)
		VALUES (1, 'legacy-sheet', 'Leads', 1, ?, NULL)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// First boot migrates it; then the user switches to Notion.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	notion, _ := json.Marshal(NotionConfig{WorkspaceID: "ws", DatabaseID: "db", DataSourceID: "ds"})
	if err := st.SetDestination(ctx, &Destination{UserID: 1, Type: "notion", Config: notion}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// A later boot must not resurrect the stale user_sheets row over the
	// user's current Notion destination.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dest, err := st.GetDestination(ctx, 1)
	if err != nil || dest.Type != "notion" {
		t.Fatalf("backfill clobbered the live destination: %+v err=%v", dest, err)
	}
	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM destinations WHERE user_id = 1`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly 1 destination row, got %d", rows)
	}
}

// A fresh database (no legacy rows to lift) opens cleanly and simply has no
// destinations yet.
func TestBackfillOnFreshDatabase(t *testing.T) {
	st := openTest(t)
	if _, err := st.GetDestination(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestMigrateOAuthAccountsRenamesGoogleSub covers the oauth_accounts
// generalization on a live database: a file written when the column was
// google_sub must come up with provider_sub, keeping every existing link
// readable. Losing this would sign every Google user out.
func TestMigrateOAuthAccountsRenamesGoogleSub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-legacy.db")
	ctx := context.Background()

	// A database as an older build left it: the column is still google_sub.
	func() {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.db.Exec(`ALTER TABLE oauth_accounts RENAME COLUMN provider_sub TO google_sub`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`
			INSERT INTO oauth_accounts (user_id, provider, google_sub, access_token_enc,
				refresh_token_enc, token_expiry, scopes, connected_at, broken_at)
			VALUES (7, 'google', 'sub-123', ?, ?, NULL, 'openid', ?, NULL)`,
			[]byte("enc-access"), []byte("enc-refresh"),
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()

	acct, err := st.GetOAuthAccount(ctx, 7, "google")
	if err != nil {
		t.Fatalf("existing google link lost by the rename: %v", err)
	}
	if acct.ProviderSub != "sub-123" || string(acct.AccessTokenEnc) != "enc-access" {
		t.Fatalf("link not preserved: %+v", acct)
	}
	// Sign-in lookup by subject still resolves.
	bySub, err := st.GetOAuthAccountBySub(ctx, "google", "sub-123")
	if err != nil || bySub.UserID != 7 {
		t.Fatalf("lookup by sub: %+v err=%v", bySub, err)
	}
	// A Notion link never satisfies a Google sign-in lookup, even with the
	// same subject value.
	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: 8, Provider: "notion", ProviderSub: "bot-123",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOAuthAccountBySub(ctx, "google", "bot-123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a notion link must not satisfy a google sign-in, got %v", err)
	}
}

// TestMigrateOAuthDropsGlobalSubUnique covers the in-place upgrade of a
// database that still carries the Google-era column-level UNIQUE on
// provider_sub. That constraint blocked a second user from connecting a Notion
// workspace another user had already connected, because Notion's subject is a
// per-install bot id rather than an identity.
//
// The rebuild must preserve every existing row, drop the global constraint,
// and keep Google subjects unique.
func TestMigrateOAuthDropsGlobalSubUnique(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-unique-legacy.db")
	ctx := context.Background()

	// A database as the previous build left it: provider_sub globally UNIQUE.
	func() {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := st.db.Exec(`DROP TABLE oauth_accounts`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`
			CREATE TABLE oauth_accounts (
				user_id           INTEGER NOT NULL,
				provider          TEXT NOT NULL,
				provider_sub      TEXT NOT NULL UNIQUE,
				access_token_enc  BLOB,
				refresh_token_enc BLOB,
				token_expiry      TEXT,
				scopes            TEXT,
				connected_at      TEXT NOT NULL,
				broken_at         TEXT,
				PRIMARY KEY (user_id, provider)
			)`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`
			INSERT INTO oauth_accounts (user_id, provider, provider_sub, access_token_enc,
				refresh_token_enc, token_expiry, scopes, connected_at, broken_at)
			VALUES (5, 'notion', 'bot-shared', ?, NULL, NULL, 'notion:granted-resources', ?, NULL)`,
			[]byte("enc-existing"), time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()

	// The pre-existing row survived the rebuild.
	acct, err := st.GetOAuthAccount(ctx, 5, "notion")
	if err != nil {
		t.Fatalf("existing row lost in migration: %v", err)
	}
	if string(acct.AccessTokenEnc) != "enc-existing" || acct.ProviderSub != "bot-shared" {
		t.Errorf("migrated row = %+v, want the original token and subject", acct)
	}

	// A second user may now connect the same Notion workspace.
	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: 6, Provider: "notion", ProviderSub: "bot-shared",
		AccessTokenEnc: []byte("enc-second"),
	}); err != nil {
		t.Fatalf("second user on the shared workspace: %v", err)
	}

	// Google subjects are still one-to-one.
	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: 8, Provider: "google", ProviderSub: "g-1", AccessTokenEnc: []byte("a"),
	}); err != nil {
		t.Fatalf("first google link: %v", err)
	}
	if err := st.UpsertOAuthAccount(ctx, &OAuthAccount{
		UserID: 9, Provider: "google", ProviderSub: "g-1", AccessTokenEnc: []byte("b"),
	}); err == nil {
		t.Fatal("two users share a Google subject after migration; want an error")
	}
}
