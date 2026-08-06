package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EOEboh/ziga-data/internal/destination"
	"github.com/EOEboh/ziga-data/internal/store"
)

// fakeNotionAPI stands in for api.notion.com at the HTTP-handler level: it
// serves a small workspace (one granted page, one granted database) and
// records the pages created in it.
type fakeNotionAPI struct {
	server *httptest.Server

	mu sync.Mutex
	// schema is the granted database's properties, as the Notion wire shape.
	schema string
	// created holds the properties payload of each page create.
	created []map[string]any
	// status, when non-zero, makes every request fail with it.
	status int
	code   string
	// versions records the Notion-Version header of every request.
	versions []string
}

const defaultNotionSchema = `{
	"Name":    {"id":"t","type":"title"},
	"Contact": {"id":"c","type":"email"},
	"Source":  {"id":"s","type":"select","select":{"options":[{"name":"referral"}]}},
	"Need":    {"id":"n","type":"rich_text"},
	"Date":    {"id":"d","type":"date"},
	"Flags":   {"id":"f","type":"rich_text"}
}`

func newFakeNotionAPI(t *testing.T) *fakeNotionAPI {
	t.Helper()
	f := &fakeNotionAPI{schema: defaultNotionSchema}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.versions = append(f.versions, r.Header.Get("Notion-Version"))
		status, code, schema := f.status, f.code, f.schema
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{
				"object": "error", "status": status, "code": code, "message": "fake failure",
			})
			return
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/search":
			filter, _ := body["filter"].(map[string]any)
			switch filter["value"] {
			case "data_source":
				w.Write([]byte(`{"results":[{"object":"data_source","id":"ds-granted",
					"title":[{"plain_text":"Client Leads"}],
					"parent":{"type":"database_id","database_id":"db-granted"}}]}`))
			default:
				w.Write([]byte(`{"results":[{"object":"page","id":"page-granted",
					"properties":{"title":{"type":"title","title":[{"plain_text":"Workspace Home"}]}}}]}`))
			}

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/databases/"):
			w.Write([]byte(`{"data_sources":[{"id":"ds-granted","name":"Client Leads"}]}`))

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/data_sources/"):
			w.Write([]byte(`{"id":"ds-granted","title":[{"plain_text":"Client Leads"}],"properties":` + schema + `}`))

		case r.Method == http.MethodPost && r.URL.Path == "/databases":
			w.Write([]byte(`{"id":"db-created","data_sources":[{"id":"ds-created"}]}`))

		case r.Method == http.MethodPost && r.URL.Path == "/pages":
			props, _ := body["properties"].(map[string]any)
			f.mu.Lock()
			f.created = append(f.created, props)
			f.mu.Unlock()
			w.Write([]byte(`{"url":"https://notion.so/page-new"}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/query"):
			w.Write([]byte(`{"results":[]}`))

		default:
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeNotionAPI) fail(status int, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.code = status, code
}

func (f *fakeNotionAPI) pages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakeNotionAPI) seenVersions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.versions))
	copy(out, f.versions)
	return out
}

// newConnectedNotionTest is a server with Notion connected and its API pointed
// at the fake, ready to pick a database.
func newConnectedNotionTest(t *testing.T) (*authTest, *fakeNotionAPI, int64) {
	t.Helper()
	a, _, uid := newNotionTest(t)
	fapi := newFakeNotionAPI(t)
	a.s.notionBaseURL = fapi.server.URL
	runNotionConnect(t, a)
	return a, fapi, uid
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestNotionResourcesListsGrants(t *testing.T) {
	a, _, _ := newConnectedNotionTest(t)
	rec := a.do("GET", "/api/notion/resources", nil, true)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)

	dbs, _ := resp["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases = %v", dbs)
	}
	db, _ := dbs[0].(map[string]any)
	// The user-facing id is the database; the data source is what writes use.
	if db["id"] != "db-granted" || db["data_source_id"] != "ds-granted" {
		t.Fatalf("database = %+v", db)
	}
	if db["title"] != "Client Leads" {
		t.Fatalf("title = %v", db["title"])
	}
	if resp["can_create"] != true {
		t.Fatal("a granted page means a database can be created")
	}
}

// Auto-create is the safe default: the app owns the schema, so the mapping is
// complete and nothing is ever dropped.
func TestNotionCreateDatabaseSetsDestination(t *testing.T) {
	a, _, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	rec := a.do("POST", "/api/notion/databases/create", map[string]string{"parent_page_id": "page-granted"}, true)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	dest, err := a.s.store.GetDestination(ctx, uid)
	if err != nil {
		t.Fatalf("destination not set: %v", err)
	}
	if dest.Type != string(destination.TypeNotion) {
		t.Fatalf("type = %q", dest.Type)
	}
	cfg, err := dest.NotionConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseID != "db-created" || cfg.DataSourceID != "ds-created" {
		t.Fatalf("config = %+v", cfg)
	}
	if !cfg.CreatedByApp {
		t.Fatal("an app-created database must be marked as such")
	}
	for _, field := range []string{"date", "name", "contact", "source", "need", "notes", "flags"} {
		if _, ok := cfg.Mapping[field]; !ok {
			t.Fatalf("auto-created database drops %q", field)
		}
	}
}

func TestNotionMappingSuggestsAndReportsUnmapped(t *testing.T) {
	a, _, _ := newConnectedNotionTest(t)
	rec := a.do("GET", "/api/notion/databases/db-granted/mapping", nil, true)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)

	mapping, _ := resp["mapping"].(map[string]any)
	name, _ := mapping["name"].(map[string]any)
	if name["name"] != "Name" || name["type"] != "title" {
		t.Fatalf("name mapping = %+v", name)
	}
	// The fake's schema has no Notes property, so notes must be reported as
	// unmapped up front rather than silently disappearing at write time.
	unmapped, _ := resp["unmapped"].([]any)
	if len(unmapped) != 1 || unmapped[0] != "notes" {
		t.Fatalf("unmapped = %v, want [notes]", unmapped)
	}
	props, _ := resp["properties"].([]any)
	if len(props) != 6 {
		t.Fatalf("properties = %d, want every property offered for adjustment", len(props))
	}
}

// The user's adjusted mapping is what gets stored, with exact property casing.
func TestNotionSetDestinationStoresAdjustedMapping(t *testing.T) {
	a, _, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	rec := a.do("POST", "/api/notion/destination", map[string]any{
		"database_id": "db-granted",
		"mapping": map[string]any{
			"name":    map[string]string{"name": "Name", "type": "title"},
			"contact": map[string]string{"name": "Contact", "type": "email"},
			// Deliberately route need into the Flags property.
			"need": map[string]string{"name": "Flags", "type": "rich_text"},
		},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	dest, _ := a.s.store.GetDestination(ctx, uid)
	cfg, err := dest.NotionConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mapping["need"].Name != "Flags" {
		t.Fatalf("adjusted mapping not stored: %+v", cfg.Mapping)
	}
	if cfg.CreatedByApp {
		t.Fatal("a picked database must not be marked app-created")
	}
	if cfg.DataSourceID != "ds-granted" {
		t.Fatalf("data source = %q, want the resolved one", cfg.DataSourceID)
	}
}

// A mapping that no longer fits the live schema is rejected at save time,
// rather than failing on the user's first real lead.
func TestNotionSetDestinationValidatesAgainstLiveSchema(t *testing.T) {
	a, _, uid := newConnectedNotionTest(t)

	for _, tc := range []struct {
		name    string
		mapping map[string]any
		want    int
	}{
		{"property does not exist", map[string]any{
			"name": map[string]string{"name": "Nope", "type": "title"},
		}, http.StatusUnprocessableEntity},
		{"wrong case is a different property", map[string]any{
			"name": map[string]string{"name": "name", "type": "title"},
		}, http.StatusUnprocessableEntity},
		{"type drifted", map[string]any{
			"name": map[string]string{"name": "Name", "type": "rich_text"},
		}, http.StatusUnprocessableEntity},
		{"unknown ziga field", map[string]any{
			"nonsense": map[string]string{"name": "Name", "type": "title"},
		}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := a.do("POST", "/api/notion/destination", map[string]any{
				"database_id": "db-granted", "mapping": tc.mapping,
			}, true)
			if rec.Code != tc.want {
				t.Fatalf("code=%d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// None of the rejected attempts may have set a destination.
	if _, err := a.s.store.GetDestination(context.Background(), uid); err == nil {
		t.Fatal("a rejected mapping must not become the destination")
	}
}

// The whole point: a confirmed lead lands as a page in the user's Notion
// database, with each value shaped for its property type.
func TestConfirmWritesLeadToNotion(t *testing.T) {
	a, fapi, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	if rec := a.do("POST", "/api/notion/databases/create",
		map[string]string{"parent_page_id": "page-granted"}, true); rec.Code != 200 {
		t.Fatalf("create database code=%d", rec.Code)
	}

	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{
		UserID: uid, ContentHash: "notion-hash", Status: store.StatusPending, Extraction: extraction,
	}
	if _, err := a.s.store.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm",
		map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != 200 {
		t.Fatalf("confirm code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["status"] != string(store.StatusWritten) {
		t.Fatalf("status = %v", resp["status"])
	}
	if _, dropped := resp["dropped_fields"]; dropped {
		t.Fatalf("the app-created database must drop nothing, got %v", resp["dropped_fields"])
	}
	if resp["url"] != "https://notion.so/page-new" {
		t.Fatalf("url = %v, want a link to the created page", resp["url"])
	}

	pages := fapi.pages()
	if len(pages) != 1 {
		t.Fatalf("pages created = %d, want 1", len(pages))
	}
	props := pages[0]
	if _, ok := props["Name"].(map[string]any)["title"]; !ok {
		t.Fatalf("Name = %+v, want a title value", props["Name"])
	}
	if props["Contact"].(map[string]any)["email"] != "jane@x.com" {
		t.Fatalf("Contact = %+v", props["Contact"])
	}

	// Every request the whole flow made carried the pinned version header.
	for i, v := range fapi.seenVersions() {
		if v != a.s.cfg.NotionVersion {
			t.Fatalf("request %d sent Notion-Version %q, want %q", i, v, a.s.cfg.NotionVersion)
		}
	}
}

// A database missing a Ziga field writes what it can and names what it
// dropped — never a silent loss.
func TestConfirmReportsDroppedFieldsFromNotion(t *testing.T) {
	a, fapi, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	// Pick the granted database, which has no Notes property.
	if rec := a.do("POST", "/api/notion/destination",
		map[string]any{"database_id": "db-granted"}, true); rec.Code != 200 {
		t.Fatalf("set destination code=%d body=%s", rec.Code, rec.Body.String())
	}

	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{
		UserID: uid, ContentHash: "notion-drop", Status: store.StatusPending, Extraction: extraction,
	}
	if _, err := a.s.store.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm",
		map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != 200 {
		t.Fatalf("a partial write must still succeed: code=%d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	dropped, _ := resp["dropped_fields"].([]any)
	if len(dropped) != 1 || dropped[0] != "notes" {
		t.Fatalf("dropped_fields = %v, want [notes]", dropped)
	}
	// The rest of the lead still landed.
	if len(fapi.pages()) != 1 {
		t.Fatal("the mapped fields must still be written")
	}
	if _, ok := fapi.pages()[0]["Need"]; !ok {
		t.Fatal("mapped fields must be present in the created page")
	}
}

// Access revoked mid-life: the destination is marked broken and the user is
// told to reconnect, rather than being left retrying forever.
func TestConfirmOnRevokedNotionPromptsReconnect(t *testing.T) {
	a, fapi, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	if rec := a.do("POST", "/api/notion/databases/create",
		map[string]string{"parent_page_id": "page-granted"}, true); rec.Code != 200 {
		t.Fatalf("create database code=%d", rec.Code)
	}

	// The user removed Ziga's access to the database in Notion.
	fapi.fail(http.StatusNotFound, "object_not_found")

	extraction, _ := json.Marshal(goodResult())
	sub := &store.Submission{
		UserID: uid, ContentHash: "notion-revoked", Status: store.StatusPending, Extraction: extraction,
	}
	if _, err := a.s.store.Insert(ctx, sub); err != nil {
		t.Fatal(err)
	}

	rec := a.do("POST", "/api/submissions/"+itoa(sub.ID)+"/confirm",
		map[string]any{"fields": map[string]string{}}, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409 reconnect (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	if resp["needs_reconnect"] != true {
		t.Fatalf("response must flag a reconnect: %v", resp)
	}

	// Both the link and the destination are flagged, so /api/me and the
	// destination picker prompt a reconnect.
	dest, err := a.s.store.GetDestination(ctx, uid)
	if err != nil || !dest.Broken() {
		t.Fatalf("destination must be marked broken: %+v err=%v", dest, err)
	}
	if a.s.notionConnected(ctx, uid) {
		t.Fatal("the notion link must be marked broken")
	}

	// The lead is not lost — it stays retryable in the queue.
	stored, _ := a.s.store.Get(ctx, uid, sub.ID)
	if stored.Status != store.StatusFailedWrite {
		t.Fatalf("submission status = %q, want failed_write so it stays retryable", stored.Status)
	}
}

// Switching destinations replaces rather than accumulates: one active
// destination at a time.
func TestSwitchingToNotionReplacesSheet(t *testing.T) {
	a, _, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	// Start on a Google Sheet.
	if err := a.s.setSheetDestination(ctx, uid, &store.SheetConfig{
		SpreadsheetID: "sheet-1", SheetTab: "Leads", CreatedByApp: true,
	}); err != nil {
		t.Fatal(err)
	}

	if rec := a.do("POST", "/api/notion/destination",
		map[string]any{"database_id": "db-granted"}, true); rec.Code != 200 {
		t.Fatalf("switch code=%d body=%s", rec.Code, rec.Body.String())
	}

	dest, _ := a.s.store.GetDestination(ctx, uid)
	if dest.Type != string(destination.TypeNotion) {
		t.Fatalf("type = %q, want notion", dest.Type)
	}
	if dest.Broken() {
		t.Fatal("a freshly connected destination must not be broken")
	}
}

// Disconnecting Google must not break a Notion destination — its access came
// from a different grant.
func TestGoogleDisconnectLeavesNotionDestinationHealthy(t *testing.T) {
	a, _, uid := newConnectedNotionTest(t)
	ctx := context.Background()

	if rec := a.do("POST", "/api/notion/destination",
		map[string]any{"database_id": "db-granted"}, true); rec.Code != 200 {
		t.Fatalf("set destination code=%d", rec.Code)
	}
	if rec := a.do("POST", "/api/auth/google/disconnect", map[string]string{}, true); rec.Code != 200 {
		t.Fatalf("disconnect code=%d", rec.Code)
	}

	dest, err := a.s.store.GetDestination(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if dest.Broken() {
		t.Fatal("disconnecting Google must not break a Notion destination")
	}
}
