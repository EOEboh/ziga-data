package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/store"
	"google.golang.org/api/option"
	_ "modernc.org/sqlite"
)

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// fakeSheets emulates the subset of the Google Sheets REST API the client
// library calls: create, values.append, values.get, and spreadsheet metadata.
type fakeSheets struct {
	server   *httptest.Server
	appends  map[string][][]string // spreadsheetID -> appended rows
	newID    string
	firstTab string
}

func newFakeSheets(t *testing.T) *fakeSheets {
	t.Helper()
	fs := &fakeSheets{appends: map[string][][]string{}, newID: "new-sheet-id", firstTab: "Sheet1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, ":append"):
			id := spreadsheetIDFromPath(path)
			var body struct {
				Values [][]any `json:"values"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			for _, row := range body.Values {
				cells := make([]string, len(row))
				for i, c := range row {
					cells[i], _ = c.(string)
				}
				fs.appends[id] = append(fs.appends[id], cells)
			}
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/v4/spreadsheets"):
			json.NewEncoder(w).Encode(map[string]any{
				"spreadsheetId": fs.newID,
				"sheets":        []map[string]any{{"properties": map[string]any{"title": "Leads"}}},
			})
		case r.Method == http.MethodGet && strings.Contains(path, "/values/"):
			id := spreadsheetIDFromPath(path)
			vals := fs.appends[id]
			out := make([][]string, len(vals))
			copy(out, vals)
			json.NewEncoder(w).Encode(map[string]any{"values": out})
		case r.Method == http.MethodGet: // spreadsheet metadata
			json.NewEncoder(w).Encode(map[string]any{
				"sheets": []map[string]any{{"properties": map[string]any{"title": fs.firstTab}}},
			})
		default:
			http.Error(w, "unhandled: "+r.Method+" "+path, http.StatusNotImplemented)
		}
	})
	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

func spreadsheetIDFromPath(path string) string {
	const p = "/v4/spreadsheets/"
	rest := strings.TrimPrefix(path, p)
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// newSheetsTest is a Google-configured server whose Google APIs (identity +
// Sheets) point at fakes, with a logged-in Google user.
func newSheetsTest(t *testing.T) (*authTest, *fakeSheets, int64) {
	t.Helper()
	a, fg := newGoogleTest(t)
	fsheets := newFakeSheets(t)
	a.s.sheetsOpts = []option.ClientOption{option.WithEndpoint(fsheets.server.URL)}
	fg.info = oauth.UserInfo{Sub: "google-sheets", Email: "owner@x.com", EmailVerified: true}
	a.runOAuthCallback(t)
	u, err := a.s.store.GetUserByEmail(context.Background(), "owner@x.com")
	if err != nil {
		t.Fatal(err)
	}
	return a, fsheets, u.ID
}

// mustSheetDestination reads back the user's destination and asserts it is a
// Google Sheet, returning its decoded config.
func mustSheetDestination(t *testing.T, a *authTest, uid int64) *store.SheetConfig {
	t.Helper()
	dest, err := a.s.store.GetDestination(context.Background(), uid)
	if err != nil {
		t.Fatalf("get destination: %v", err)
	}
	if dest.Type != string(destination.TypeGoogleSheet) {
		t.Fatalf("destination type = %q, want google_sheet", dest.Type)
	}
	cfg, err := dest.SheetConfig()
	if err != nil {
		t.Fatalf("decode sheet config: %v", err)
	}
	return cfg
}

func TestSheetsCreateStoresDestination(t *testing.T) {
	a, _, uid := newSheetsTest(t)
	rec := a.do("POST", "/api/sheets/create", map[string]string{}, true)
	if rec.Code != 200 {
		t.Fatalf("create code=%d", rec.Code)
	}
	sheet := mustSheetDestination(t, a, uid)
	if sheet.SpreadsheetID != "new-sheet-id" || !sheet.CreatedByApp {
		t.Fatalf("sheet destination not stored: %+v", sheet)
	}
}

func TestSheetsAttachStoresExistingSheet(t *testing.T) {
	a, fsheets, uid := newSheetsTest(t)
	fsheets.firstTab = "Contacts"
	rec := a.do("POST", "/api/sheets/attach", map[string]string{"spreadsheet_id": "existing-123"}, true)
	if rec.Code != 200 {
		t.Fatalf("attach code=%d", rec.Code)
	}
	sheet := mustSheetDestination(t, a, uid)
	if sheet.SpreadsheetID != "existing-123" || sheet.SheetTab != "Contacts" || sheet.CreatedByApp {
		t.Fatalf("attached sheet wrong: %+v", sheet)
	}
}

func TestConfirmWritesToUsersOwnSheet(t *testing.T) {
	a, fsheets, uid := newSheetsTest(t)
	ctx := context.Background()
	// Connect a destination.
	if rec := a.do("POST", "/api/sheets/create", map[string]string{}, true); rec.Code != 200 {
		t.Fatalf("create code=%d", rec.Code)
	}
	// Seed a pending submission owned by the user.
	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{UserID: uid, ContentHash: "hash-1", Status: store.StatusPending, Extraction: extraction}
	if _, err := a.s.store.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm", map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != 200 {
		t.Fatalf("confirm code=%d body=%s", rec.Code, rec.Body.String())
	}
	// The row landed in the user's own spreadsheet (header + data row).
	rows := fsheets.appends["new-sheet-id"]
	if len(rows) < 1 {
		t.Fatalf("expected an append to the user's sheet, got %v", fsheets.appends)
	}
}

func TestConfirmWithoutSheetPromptsSetup(t *testing.T) {
	a, _, uid := newSheetsTest(t)
	ctx := context.Background()
	// No sheet connected.
	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{UserID: uid, ContentHash: "hash-2", Status: store.StatusPending, Extraction: extraction}
	a.s.store.Insert(ctx, sub)

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm", map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm without a sheet: code=%d, want 409", rec.Code)
	}
}

func TestConfirmWithBrokenConnectionPromptsReconnect(t *testing.T) {
	a, _, uid := newSheetsTest(t)
	ctx := context.Background()
	a.do("POST", "/api/sheets/create", map[string]string{}, true)
	// Simulate a revoked grant.
	a.s.store.MarkOAuthBroken(ctx, uid, "google")

	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{UserID: uid, ContentHash: "hash-3", Status: store.StatusPending, Extraction: extraction}
	a.s.store.Insert(ctx, sub)

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm", map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm with broken connection: code=%d, want 409 reconnect", rec.Code)
	}
}

// TestLegacySheetUserWritesAfterMigration is the regression guarantee for the
// destination-model generalization: a user who connected a Google Sheet under
// the previous build — a user_sheets row and no destinations row — must keep
// writing to that same spreadsheet after upgrading, with no reconnect.
//
// It walks the whole path: seed a database the way the old binary left it,
// boot the server on it (which migrates), sign in, confirm a lead, and assert
// the row landed on the original spreadsheet.
func TestLegacySheetUserWritesAfterMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-user.db")

	// A database as the previous build left it: a verified user with a
	// connected sheet recorded in user_sheets, and no destinations table.
	// The legacy state is written over the raw file, since this build has no
	// Go accessors for user_sheets any more.
	func() {
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		u, err := st.CreateUser(ctx, "owner@x.com", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkEmailVerified(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		st.Close()

		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`DROP TABLE destinations`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO user_sheets (user_id, spreadsheet_id, sheet_tab, created_by_app, connected_at, broken_at)
			VALUES (?, 'legacy-sheet-id', 'Leads', 1, ?, NULL)`,
			u.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}()

	// Boot on that database — store.Open migrates it — and sign the user in.
	a, fg := newGoogleTestAt(t, dbPath)
	fsheets := newFakeSheets(t)
	a.s.sheetsOpts = []option.ClientOption{option.WithEndpoint(fsheets.server.URL)}
	fg.info = oauth.UserInfo{Sub: "legacy-google-sub", Email: "owner@x.com", EmailVerified: true}
	a.runOAuthCallback(t)

	u, err := a.s.store.GetUserByEmail(ctx, "owner@x.com")
	if err != nil {
		t.Fatal(err)
	}

	// The migrated destination points at the original spreadsheet, and the
	// user is not asked to reconnect or re-onboard.
	sheet := mustSheetDestination(t, a, u.ID)
	if sheet.SpreadsheetID != "legacy-sheet-id" || sheet.SheetTab != "Leads" {
		t.Fatalf("migrated destination = %+v, want legacy-sheet-id/Leads", sheet)
	}
	if !a.s.destinationConnected(ctx, u.ID) {
		t.Fatal("a migrated user must read as connected, not sent back to onboarding")
	}

	// A confirmed lead still lands on that same spreadsheet.
	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{
		UserID: u.ID, ContentHash: "legacy-hash", Status: store.StatusPending, Extraction: extraction,
	}
	if _, err := a.s.store.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}
	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm", map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != 200 {
		t.Fatalf("confirm code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rows := fsheets.appends["legacy-sheet-id"]; len(rows) == 0 {
		t.Fatalf("lead did not reach the pre-existing sheet; appends=%v", fsheets.appends)
	}
}
