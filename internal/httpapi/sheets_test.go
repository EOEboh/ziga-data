package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/EOEboh/ziga-data/internal/oauth"
	"github.com/EOEboh/ziga-data/internal/store"
	"google.golang.org/api/option"
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

func TestSheetsCreateStoresDestination(t *testing.T) {
	a, _, uid := newSheetsTest(t)
	rec := a.do("POST", "/api/sheets/create", map[string]string{}, true)
	if rec.Code != 200 {
		t.Fatalf("create code=%d", rec.Code)
	}
	sheet, err := a.s.store.GetUserSheet(context.Background(), uid)
	if err != nil || sheet.SpreadsheetID != "new-sheet-id" || !sheet.CreatedByApp {
		t.Fatalf("user sheet not stored: %+v err=%v", sheet, err)
	}
}

func TestSheetsAttachStoresExistingSheet(t *testing.T) {
	a, fsheets, uid := newSheetsTest(t)
	fsheets.firstTab = "Contacts"
	rec := a.do("POST", "/api/sheets/attach", map[string]string{"spreadsheet_id": "existing-123"}, true)
	if rec.Code != 200 {
		t.Fatalf("attach code=%d", rec.Code)
	}
	sheet, err := a.s.store.GetUserSheet(context.Background(), uid)
	if err != nil || sheet.SpreadsheetID != "existing-123" || sheet.SheetTab != "Contacts" || sheet.CreatedByApp {
		t.Fatalf("attached sheet wrong: %+v err=%v", sheet, err)
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
