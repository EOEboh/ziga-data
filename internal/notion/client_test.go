package notion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EOEboh/ziga-data/internal/destination"
)

// fakeAPI stands in for api.notion.com. It records every request so tests can
// assert on headers and payloads, with no network involved.
type fakeAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	// handler, when set, serves the request; otherwise a default is used.
	handler func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

type recordedRequest struct {
	Method  string
	Path    string
	Version string
	Auth    string
	Body    map[string]any
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path,
			Version: r.Header.Get("Notion-Version"),
			Auth:    r.Header.Get("Authorization"),
			Body:    body,
		})
		handler := f.handler
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if handler != nil {
			handler(w, r, body)
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) seen() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeAPI) setHandler(h func(w http.ResponseWriter, r *http.Request, body map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

const testVersion = "2026-03-11"

func newTestClient(t *testing.T, f *fakeAPI, workspace string) *Client {
	t.Helper()
	c, err := New("ntn-token", workspace, testVersion, f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Notion requires Notion-Version on EVERY request. A single call site that
// forgets it fails in production only for that endpoint, so this drives a
// range of calls and asserts the header on all of them.
func TestNotionVersionHeaderOnEveryRequest(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1x"):
		case strings.Contains(r.URL.Path, "/databases/") && r.Method == http.MethodGet:
			w.Write([]byte(`{"data_sources":[{"id":"ds-1","name":"Leads"}]}`))
		case strings.Contains(r.URL.Path, "/data_sources/") && r.Method == http.MethodGet:
			w.Write([]byte(`{"id":"ds-1","title":[{"plain_text":"Leads"}],"properties":{"Name":{"id":"t","type":"title"}}}`))
		default:
			w.Write([]byte(`{"results":[],"id":"new","data_sources":[{"id":"ds-1"}],"url":"https://notion.so/p"}`))
		}
	})
	c := newTestClient(t, f, "ws-version")
	ctx := context.Background()

	// One call of each shape the destination flow makes.
	if _, err := c.ResolveDataSource(ctx, "db-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetDataSource(ctx, "ds-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GrantedResources(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CreateDatabase(ctx, "page-1", "Ziga Leads", LeadsDatabaseSchema()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePage(ctx, "ds-1", map[string]any{"Name": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryRecent(ctx, "ds-1", 3); err != nil {
		t.Fatal(err)
	}
	if err := c.AddSelectOption(ctx, "ds-1", Property{Name: "Source", Type: TypeSelect}, "X DM"); err != nil {
		t.Fatal(err)
	}

	reqs := f.seen()
	if len(reqs) < 7 {
		t.Fatalf("expected at least 7 requests, got %d", len(reqs))
	}
	for _, req := range reqs {
		if req.Version != testVersion {
			t.Errorf("%s %s: Notion-Version = %q, want %q", req.Method, req.Path, req.Version, testVersion)
		}
		if req.Auth != "Bearer ntn-token" {
			t.Errorf("%s %s: Authorization = %q", req.Method, req.Path, req.Auth)
		}
	}
}

// A 429 must be retried after honoring Retry-After, not surfaced to the user.
func TestRateLimitRetriesAfter429(t *testing.T) {
	f := newFakeAPI(t)
	var calls int
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		calls++
		if calls == 1 {
			// A sub-second Retry-After keeps the test fast while still
			// exercising the header path.
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"object":"error","code":"rate_limited","message":"slow down"}`))
			return
		}
		w.Write([]byte(`{"url":"https://notion.so/page-1"}`))
	})
	c := newTestClient(t, f, "ws-429")

	url, err := c.CreatePage(context.Background(), "ds-1", map[string]any{"Name": map[string]any{}})
	if err != nil {
		t.Fatalf("a 429 must be retried, got: %v", err)
	}
	if url != "https://notion.so/page-1" {
		t.Fatalf("url = %q", url)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want one retry after the 429", calls)
	}
}

// Exhausting retries against a persistent 429 surfaces ErrRateLimited rather
// than a bare "failed".
func TestRateLimitGivesUpAsTyped(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Retry-After", "0.01")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"rate_limited"}`))
	})
	c := newTestClient(t, f, "ws-429-hard")

	_, err := c.CreatePage(context.Background(), "ds-1", map[string]any{"Name": map[string]any{}})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// A Notion 404 usually means "the integration was never granted this
// resource", not "the resource does not exist". Misreading it would tell users
// their database was deleted instead of prompting a reconnect.
func TestNotFoundIsTreatedAsPermission(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"object":"error","status":404,"code":"object_not_found",
			"message":"Could not find data source with ID ds-1. Make sure the relevant pages and databases are shared with your integration."}`))
	})
	c := newTestClient(t, f, "ws-404")

	_, err := c.GetDataSource(context.Background(), "ds-1")
	if !errors.Is(err, ErrNoAccess) {
		t.Fatalf("err = %v, want ErrNoAccess", err)
	}
	if !NeedsReconnect(err) {
		t.Fatal("an ungranted resource must drive a reconnect prompt")
	}
}

func TestUnauthorizedIsTypedAndNeedsReconnect(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"object":"error","status":401,"code":"unauthorized","message":"API token is invalid."}`))
	})
	c := newTestClient(t, f, "ws-401")

	_, err := c.GetDataSource(context.Background(), "ds-1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if !NeedsReconnect(err) {
		t.Fatal("a revoked token must drive a reconnect prompt")
	}
}

// A 400 is the client's fault and must not be retried — retrying a rejected
// payload just burns the rate limit.
func TestValidationErrorIsNotRetried(t *testing.T) {
	f := newFakeAPI(t)
	var calls int
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"object":"error","status":400,"code":"validation_error","message":"bad"}`))
	})
	c := newTestClient(t, f, "ws-400")

	_, err := c.CreatePage(context.Background(), "ds-1", map[string]any{})
	if err == nil || !IsValidation(err) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want no retry on a 400", calls)
	}
}

// The client must build the data-source shaped requests the pinned API version
// expects: a data_source_id parent on page create, and the schema nested under
// initial_data_source on database create.
func TestRequestsUseDataSourceModel(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Write([]byte(`{"id":"db-new","data_sources":[{"id":"ds-new"}],"url":"https://notion.so/p"}`))
	})
	c := newTestClient(t, f, "ws-shape")
	ctx := context.Background()

	if _, err := c.CreatePage(ctx, "ds-1", map[string]any{"Name": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	dbID, dsID, err := c.CreateDatabase(ctx, "page-1", "Ziga Leads", LeadsDatabaseSchema())
	if err != nil {
		t.Fatal(err)
	}
	if dbID != "db-new" || dsID != "ds-new" {
		t.Fatalf("create returned db=%q ds=%q", dbID, dsID)
	}

	reqs := f.seen()
	pageReq := reqs[0]
	parent, _ := pageReq.Body["parent"].(map[string]any)
	if parent["type"] != "data_source_id" || parent["data_source_id"] != "ds-1" {
		t.Fatalf("page parent = %+v, want a data_source_id parent", parent)
	}

	dbReq := reqs[1]
	if _, ok := dbReq.Body["initial_data_source"]; !ok {
		t.Fatalf("database create body = %+v, want the schema under initial_data_source", dbReq.Body)
	}
	if _, ok := dbReq.Body["properties"]; ok {
		t.Fatal("database create must not put properties at the top level")
	}
}

// --- writer behavior ---

// testWriter wires a Writer to the fake with the mapping the app's own
// created database uses.
func testWriter(t *testing.T, f *fakeAPI, workspace string, mapping Mapping) *Writer {
	t.Helper()
	return NewWriter(newTestClient(t, f, workspace), "ds-1", mapping,
		[]string{"date", "name", "contact", "source", "need", "notes", "flags"})
}

func sampleLead() destination.Lead {
	return destination.Lead{Cells: []destination.Cell{
		{Field: "date", Value: "2026-07-15"},
		{Field: "name", Value: "Ada Okafor"},
		{Field: "contact", Value: "ada@lumen.studio"},
		{Field: "source", Value: "X DM"},
		{Field: "need", Value: "Wants a landing page"},
		{Field: "notes", Value: "Budget around $1,200"},
		{Field: "flags", Value: ""},
	}}
}

// Each value must land in its mapped property, shaped for that property's type.
func TestWriterCoercesPerPropertyType(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Write([]byte(`{"url":"https://notion.so/page-1"}`))
	})
	wr := testWriter(t, f, "ws-write", LeadsDatabaseMapping())

	res, err := wr.Write(context.Background(), sampleLead())
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://notion.so/page-1" {
		t.Fatalf("URL = %q", res.URL)
	}
	if len(res.Dropped) != 0 {
		t.Fatalf("nothing should be dropped, got %v", res.Dropped)
	}

	props, _ := f.seen()[0].Body["properties"].(map[string]any)
	// title -> title spans
	title, _ := props["Name"].(map[string]any)
	if _, ok := title["title"]; !ok {
		t.Fatalf("Name = %+v, want a title value", title)
	}
	// email -> a bare string
	email, _ := props["Contact"].(map[string]any)
	if email["email"] != "ada@lumen.studio" {
		t.Fatalf("Contact = %+v, want an email value", email)
	}
	// select -> {name}
	sel, _ := props["Source"].(map[string]any)
	selVal, _ := sel["select"].(map[string]any)
	if selVal["name"] != "X DM" {
		t.Fatalf("Source = %+v, want a select option", sel)
	}
	// date -> {start}
	date, _ := props["Date"].(map[string]any)
	dateVal, _ := date["date"].(map[string]any)
	if dateVal["start"] != "2026-07-15" {
		t.Fatalf("Date = %+v, want a date value", date)
	}
	// An empty field is omitted rather than written as empty.
	if _, ok := props["Flags"]; ok {
		t.Fatal("an empty field must not be written")
	}
}

// A database missing a property Ziga wants must not fail the whole write: the
// mapped fields land and the dropped ones are named.
func TestWriterReportsDroppedFields(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Write([]byte(`{"url":"https://notion.so/page-2"}`))
	})
	// This database has nowhere for notes or source.
	wr := testWriter(t, f, "ws-partial", Mapping{
		"name":    {Name: "Name", Type: TypeTitle},
		"contact": {Name: "Contact", Type: TypeEmail},
		"date":    {Name: "Date", Type: TypeDate},
		"need":    {Name: "Need", Type: TypeRichText},
	})

	res, err := wr.Write(context.Background(), sampleLead())
	if err != nil {
		t.Fatalf("a partial write must still succeed: %v", err)
	}
	want := []string{"notes", "source"}
	if strings.Join(res.Dropped, ",") != strings.Join(want, ",") {
		t.Fatalf("Dropped = %v, want %v", res.Dropped, want)
	}
	// What did map still got written.
	props, _ := f.seen()[0].Body["properties"].(map[string]any)
	if _, ok := props["Name"]; !ok {
		t.Fatal("mapped fields must still be written")
	}
}

// A value that cannot live in its mapped property (a handle in an email
// property) is dropped and reported, never coerced into something wrong.
func TestWriterDropsUncoercibleValue(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Write([]byte(`{"url":"https://notion.so/page-3"}`))
	})
	wr := testWriter(t, f, "ws-coerce", LeadsDatabaseMapping())

	lead := sampleLead()
	lead.Cells[2].Value = "@adaokafor" // contact is a handle, not an email
	res, err := wr.Write(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != "contact" {
		t.Fatalf("Dropped = %v, want [contact]", res.Dropped)
	}
	props, _ := f.seen()[0].Body["properties"].(map[string]any)
	if _, ok := props["Contact"]; ok {
		t.Fatal("an uncoercible value must not be written at all")
	}
}

// When nothing maps, the page would be blank — that is a failure, not a
// partial success, so the lead stays in the queue for a retry.
func TestWriterFailsWhenNothingMaps(t *testing.T) {
	f := newFakeAPI(t)
	wr := testWriter(t, f, "ws-nothing", Mapping{})

	if _, err := wr.Write(context.Background(), sampleLead()); err == nil {
		t.Fatal("a write with no mapped fields must fail")
	}
	if len(f.seen()) != 0 {
		t.Fatal("no request should be made when nothing maps")
	}
}

// A select value Notion has not seen: the write is rejected, the option is
// added to the schema, and the write is retried once. This holds whether or
// not Notion creates options implicitly.
func TestWriterCreatesMissingSelectOption(t *testing.T) {
	f := newFakeAPI(t)
	var pageCreates, patches int
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/pages":
			pageCreates++
			if pageCreates == 1 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"object":"error","status":400,"code":"validation_error",
					"message":"X DM is not a valid select option"}`))
				return
			}
			w.Write([]byte(`{"url":"https://notion.so/page-4"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/data_sources/"):
			// The schema knows nothing about "X DM" yet.
			w.Write([]byte(`{"id":"ds-1","properties":{
				"Name":{"id":"t","type":"title"},
				"Source":{"id":"s","type":"select","select":{"options":[{"name":"referral"}]}}}}`))
		case r.Method == http.MethodPatch:
			patches++
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	wr := testWriter(t, f, "ws-select", Mapping{
		"name":   {Name: "Name", Type: TypeTitle},
		"source": {Name: "Source", Type: TypeSelect},
	})

	res, err := wr.Write(context.Background(), sampleLead())
	if err != nil {
		t.Fatalf("an unknown select option must be repaired, got: %v", err)
	}
	if res.URL != "https://notion.so/page-4" {
		t.Fatalf("URL = %q", res.URL)
	}
	if patches != 1 {
		t.Fatalf("patches = %d, want the option added to the schema once", patches)
	}
	if pageCreates != 2 {
		t.Fatalf("page creates = %d, want exactly one retry", pageCreates)
	}

	// The patch must preserve the existing options, not replace them.
	var patchBody map[string]any
	for _, req := range f.seen() {
		if req.Method == http.MethodPatch {
			patchBody = req.Body
		}
	}
	props, _ := patchBody["properties"].(map[string]any)
	source, _ := props["Source"].(map[string]any)
	sel, _ := source["select"].(map[string]any)
	opts, _ := sel["options"].([]any)
	if len(opts) != 2 {
		t.Fatalf("options = %+v, want the existing option kept alongside the new one", opts)
	}
}

// A validation error unrelated to select options must not loop: one repair
// attempt at most, then the error surfaces.
func TestWriterDoesNotRetryUnrelatedValidationError(t *testing.T) {
	f := newFakeAPI(t)
	var pageCreates int
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.Method == http.MethodPost && r.URL.Path == "/pages" {
			pageCreates++
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"validation_error","message":"nope"}`))
			return
		}
		// The schema already contains the option, so no repair applies.
		w.Write([]byte(`{"id":"ds-1","properties":{
			"Source":{"id":"s","type":"select","select":{"options":[{"name":"X DM"}]}}}}`))
	})
	wr := testWriter(t, f, "ws-novalid", Mapping{
		"source": {Name: "Source", Type: TypeSelect},
	})

	if _, err := wr.Write(context.Background(), sampleLead()); err == nil {
		t.Fatal("want the validation error to surface")
	}
	if pageCreates != 1 {
		t.Fatalf("page creates = %d, want no retry when the option already exists", pageCreates)
	}
}

// The preview strip reads Notion pages back into schema column order.
func TestWriterRecentRendersColumnOrder(t *testing.T) {
	f := newFakeAPI(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Write([]byte(`{"results":[
			{"properties":{
				"Name":{"type":"title","title":[{"plain_text":"Newest"}]},
				"Contact":{"type":"email","email":"new@x.com"},
				"Date":{"type":"date","date":{"start":"2026-07-16"}}}},
			{"properties":{
				"Name":{"type":"title","title":[{"plain_text":"Older"}]},
				"Contact":{"type":"email","email":"old@x.com"},
				"Date":{"type":"date","date":{"start":"2026-07-15"}}}}
		]}`))
	})
	wr := testWriter(t, f, "ws-recent", LeadsDatabaseMapping())

	rows, err := wr.Recent(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Columns are date, name, contact, source, need, notes, flags — and Notion
	// returns newest first, so the preview must show oldest first.
	if rows[0][0] != "2026-07-15" || rows[0][1] != "Older" || rows[0][2] != "old@x.com" {
		t.Fatalf("first row = %v, want the older lead in column order", rows[0])
	}
	if rows[1][1] != "Newest" {
		t.Fatalf("second row = %v, want the newest lead last", rows[1])
	}
	// An unmapped column renders empty rather than shifting the row.
	if len(rows[0]) != 7 {
		t.Fatalf("row width = %d, want one cell per schema column", len(rows[0]))
	}
}

// The version must be required: a client built without one would send requests
// Notion rejects.
func TestNewRequiresVersionAndToken(t *testing.T) {
	if _, err := New("", "ws", testVersion, ""); err == nil {
		t.Fatal("a client without a token must be refused")
	}
	if _, err := New("tok", "ws", "", ""); err == nil {
		t.Fatal("a client without an API version must be refused")
	}
}
