package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	"github.com/EOEboh/ziga-data/internal/store"
)

// TestEndpointDataIsolation is the core multi-tenant guarantee at the HTTP
// layer: two users, each with submissions, and every read/write endpoint must
// refuse cross-user access. A user hitting another user's submission id gets
// 404 (not 403 — ids stay non-enumerable), and list endpoints never leak.
func TestEndpointDataIsolation(t *testing.T) {
	a := newAuthTest(t)
	ctx := context.Background()

	userA := mustVerifiedUser(t, a, "a@x.com")
	userB := mustVerifiedUser(t, a, "b@x.com")
	sessA := mustSession(t, a, userA)
	sessB := mustSession(t, a, userB)
	csrf := a.cookies[csrfCookie]

	// A pending submission for each user; A's carries an image.
	extraction, _ := json.Marshal(goodResult())
	subA := &store.Submission{
		UserID: userA, ContentHash: "iso-a", Status: store.StatusPending, Extraction: extraction,
		InputImage: []byte{0x89, 0x50}, InputImageType: "image/png",
	}
	subB := &store.Submission{UserID: userB, ContentHash: "iso-b", Status: store.StatusPending, Extraction: extraction}
	if _, err := a.s.store.Insert(ctx, subA); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.store.Insert(ctx, subB); err != nil {
		t.Fatal(err)
	}

	req := func(session, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return a.reqAs(session, csrf, method, path, body)
	}

	// --- image: owner 200, other user 404 (not 403) ---
	if rec := req(sessA, "GET", "/api/submissions/"+itoa(subA.ID)+"/image", nil); rec.Code != 200 {
		t.Fatalf("owner reading own image: code=%d, want 200", rec.Code)
	}
	if rec := req(sessB, "GET", "/api/submissions/"+itoa(subA.ID)+"/image", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user image: code=%d, want 404", rec.Code)
	}

	// --- confirm: cross-user is 404 and writes nothing ---
	if rec := req(sessB, "POST", "/api/submissions/"+itoa(subA.ID)+"/confirm", map[string]any{"fields": map[string]string{}}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user confirm: code=%d, want 404", rec.Code)
	}

	// --- discard: cross-user is 404 and leaves the row pending ---
	if rec := req(sessB, "POST", "/api/submissions/"+itoa(subA.ID)+"/discard", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user discard: code=%d, want 404", rec.Code)
	}
	if got, _ := a.s.store.Get(ctx, userA, subA.ID); got == nil || got.Status != store.StatusPending {
		t.Fatalf("cross-user discard leaked: %+v", got)
	}

	// --- queue: each user sees only their own item ---
	assertQueueOnly(t, req(sessA, "GET", "/api/queue", nil), subA.ID)
	assertQueueOnly(t, req(sessB, "GET", "/api/queue", nil), subB.ID)

	// --- history: A confirms; only A sees it ---
	if rec := req(sessA, "POST", "/api/submissions/"+itoa(subA.ID)+"/confirm", map[string]any{"fields": map[string]string{}}); rec.Code != 200 {
		t.Fatalf("owner confirm: code=%d body=%s", rec.Code, rec.Body.String())
	}
	assertHistoryCount(t, req(sessA, "GET", "/api/history", nil), 1)
	assertHistoryCount(t, req(sessB, "GET", "/api/history", nil), 0)

	// A's own confirmed id is still 404 for B on discard.
	if rec := req(sessB, "POST", "/api/submissions/"+itoa(subA.ID)+"/discard", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user discard of confirmed row: code=%d, want 404", rec.Code)
	}
}

func mustVerifiedUser(t *testing.T, a *authTest, email string) int64 {
	t.Helper()
	u, err := a.s.store.CreateUser(context.Background(), email, "")
	if err != nil {
		t.Fatal(err)
	}
	a.s.store.MarkEmailVerified(context.Background(), u.ID)
	return u.ID
}

func mustSession(t *testing.T, a *authTest, uid int64) string {
	t.Helper()
	token, _ := auth.RandomToken()
	if err := a.s.store.CreateSession(context.Background(), auth.HashToken(token), uid, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return token
}

// reqAs issues a request as a specific session with a valid CSRF pair.
func (a *authTest) reqAs(session, csrf, method, path string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var rec = httptest.NewRecorder()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	r.Header.Set("X-CSRF-Token", csrf)
	a.h.ServeHTTP(rec, r)
	return rec
}

func assertQueueOnly(t *testing.T, rec *httptest.ResponseRecorder, wantID int64) {
	t.Helper()
	var body struct {
		Count int `json:"count"`
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != 1 || body.Items[0].ID != wantID {
		t.Fatalf("queue leak: got %+v, want only id %d", body.Items, wantID)
	}
}

func assertHistoryCount(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Items) != want {
		t.Fatalf("history count = %d, want %d", len(body.Items), want)
	}
}

// TestNotionDestinationIsolation is the multi-tenant guarantee for the Notion
// destination: connecting a workspace is per-user, and one account can never
// read, write to, or disconnect another's.
//
// The dangerous shape here is a user-supplied database id: user B knowing A's
// database id must not be enough to reach it, because the request is
// authorized by B's own token, which was never granted that database.
func TestNotionDestinationIsolation(t *testing.T) {
	a, fapi, userA := newConnectedNotionTest(t)
	ctx := context.Background()

	// A connects a Notion destination.
	if rec := a.do("POST", "/api/notion/destination",
		map[string]any{"database_id": "db-granted"}, true); rec.Code != 200 {
		t.Fatalf("A set destination: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// B is a second account on the same server, with no Notion connection.
	userB := mustVerifiedUser(t, a, "b-notion@x.com")
	sessB := mustSession(t, a, userB)
	csrf := a.cookies[csrfCookie]
	asB := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return a.reqAs(sessB, csrf, method, path, body)
	}

	// Every Notion route refuses B: no link of their own means no access,
	// regardless of what ids they supply.
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/notion/resources", nil},
		{"GET", "/api/notion/databases/db-granted/mapping", nil},
		{"POST", "/api/notion/destination", map[string]any{"database_id": "db-granted"}},
		{"POST", "/api/notion/databases/create", map[string]any{"parent_page_id": "page-granted"}},
	} {
		rec := asB(route.method, route.path, route.body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s %s as an unconnected user: code=%d, want 409 connect-your-workspace",
				route.method, route.path, rec.Code)
		}
	}

	// B's attempts changed nothing: B still has no destination, and A's is
	// untouched.
	if _, err := a.s.store.GetDestination(ctx, userB); err == nil {
		t.Fatal("B must not have acquired a destination")
	}
	destA, err := a.s.store.GetDestination(ctx, userA)
	if err != nil || destA.Broken() {
		t.Fatalf("A's destination was disturbed: %+v err=%v", destA, err)
	}

	// B disconnecting Notion only ever affects B.
	if rec := asB("POST", "/api/notion/disconnect", map[string]string{}); rec.Code != 200 {
		t.Fatalf("B disconnect: code=%d", rec.Code)
	}
	if !a.s.notionConnected(ctx, userA) {
		t.Fatal("B's disconnect must not drop A's Notion link")
	}

	// A confirmed lead goes to A's database and nowhere else. B has no
	// destination, so B's confirm is refused rather than falling through to
	// anyone else's.
	extraction, _ := json.Marshal(goodResult())
	subB := &store.Submission{
		UserID: userB, ContentHash: "iso-notion-b", Status: store.StatusPending, Extraction: extraction,
	}
	if _, err := a.s.store.Insert(ctx, subB); err != nil {
		t.Fatal(err)
	}
	before := len(fapi.pages())
	if rec := asB("POST", "/api/submissions/"+itoa(subB.ID)+"/confirm",
		map[string]any{"fields": map[string]string{}}); rec.Code != http.StatusConflict {
		t.Fatalf("B confirm without a destination: code=%d, want 409", rec.Code)
	}
	if got := len(fapi.pages()); got != before {
		t.Fatalf("B's confirm wrote %d page(s) into A's workspace", got-before)
	}
}

// enableIngestion turns email capture on for an existing authTest and rebuilds
// the route tree, since Handler snapshots which routes are mounted.
func enableIngestion(t *testing.T, a *authTest) *fakeRoutes {
	t.Helper()
	a.s.cfg.InboundEmailDomain = testInboundDoma
	a.s.cfg.IngestSharedSecret = testIngestSecret
	a.s.cfg.CloudflareAPIToken = "token"
	a.s.cfg.CloudflareZoneID = "zone"
	a.s.cfg.IngestWorkerName = "worker"
	a.s.cfg.IngestDailyCap = 50
	a.s.cfg.IngestBurst = 10
	a.s.cfg.IngestMaxAddresses = 180
	a.s.ingestLimiter = newIPLimiterBurst(60, 10)
	routes := newFakeRoutes()
	a.s.cfRoutes = routes
	a.h = a.s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}})
	return routes
}

// TestIngestionDataIsolation extends the tenant guarantee to the email path.
//
// This is the one that matters most for ingestion: a lead now arrives without
// anyone being present to notice where it went. If capture attributed mail to
// the wrong tenant, nobody would catch it until a stranger's lead showed up in
// someone's spreadsheet.
func TestIngestionDataIsolation(t *testing.T) {
	a := newAuthTest(t)
	enableIngestion(t, a)
	ctx := context.Background()

	userA := mustVerifiedUser(t, a, "iso-a@x.com")
	userB := mustVerifiedUser(t, a, "iso-b@x.com")
	sessA := mustSession(t, a, userA)
	sessB := mustSession(t, a, userB)
	csrf := a.cookies[csrfCookie]

	addrA := enableInbound(t, a.s, userA)
	addrB := enableInbound(t, a.s, userB)
	if addrA == addrB {
		t.Fatal("two users must never share a capture address")
	}

	// --- each user sees only their own address ---
	var gotA, gotB inboundResponse
	json.Unmarshal(a.reqAs(sessA, csrf, "GET", "/api/inbound", nil).Body.Bytes(), &gotA)
	json.Unmarshal(a.reqAs(sessB, csrf, "GET", "/api/inbound", nil).Body.Bytes(), &gotB)
	if gotA.Address != addrA || gotB.Address != addrB {
		t.Fatalf("addresses crossed tenants: A saw %q (want %q), B saw %q (want %q)",
			gotA.Address, addrA, gotB.Address, addrB)
	}

	// --- a lead mailed to A's address belongs to A and is invisible to B ---
	rec, out := postIngest(t, a.h, mailTo(addrA))
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("ingest to A: %d %+v", rec.Code, out)
	}
	if sub, err := a.s.store.Get(ctx, userA, out.ID); err != nil || sub == nil {
		t.Fatalf("A must own the captured lead: %v %v", sub, err)
	}
	if sub, err := a.s.store.Get(ctx, userB, out.ID); err != nil || sub != nil {
		t.Fatalf("B must not be able to read A's captured lead: %v %v", sub, err)
	}

	// The queue endpoint is the surface the user actually looks at.
	var queueB struct {
		Items []submissionResponse `json:"items"`
	}
	json.Unmarshal(a.reqAs(sessB, csrf, "GET", "/api/queue", nil).Body.Bytes(), &queueB)
	for _, item := range queueB.Items {
		if item.ID == out.ID {
			t.Fatal("A's captured lead appeared in B's review queue")
		}
	}

	// --- quarantine rows are scoped too ---
	evA := &store.IngestionEvent{
		UserID: userA, Status: store.EventQuarantined, Reason: "machine_mail",
		FromAddress: "news@example.com", Subject: "roundup", BodyExcerpt: "hello",
	}
	if err := a.s.store.InsertEvent(ctx, evA); err != nil {
		t.Fatal(err)
	}
	if got, err := a.s.store.GetEvent(ctx, userB, evA.ID); err != nil || got != nil {
		t.Fatalf("B must not read A's quarantined mail: %v %v", got, err)
	}
	if n, err := a.s.store.CountEvents(ctx, userB, store.EventQuarantined); err != nil || n != 0 {
		t.Fatalf("B's quarantine count leaked A's rows: %d %v", n, err)
	}

	// --- rotation is per-tenant: A rotating must not disturb B ---
	if rec := a.reqAs(sessA, csrf, "POST", "/api/inbound/rotate", nil); rec.Code != http.StatusOK {
		t.Fatalf("A rotate: %d %s", rec.Code, rec.Body)
	}
	stillB, err := a.s.store.ActiveInboundAddress(ctx, userB)
	if err != nil || stillB == nil {
		t.Fatalf("B lost their address when A rotated: %v %v", stillB, err)
	}
	if stillB.LocalPart+"@"+testInboundDoma != addrB {
		t.Errorf("B's address changed when A rotated: %q", stillB.LocalPart)
	}

	// --- B's address still captures to B after A's rotation ---
	rec, out = postIngest(t, a.h, mailTo(addrB))
	if rec.Code != http.StatusAccepted || out.Status != ingestAccepted {
		t.Fatalf("ingest to B after A rotated: %d %+v", rec.Code, out)
	}
	if sub, err := a.s.store.Get(ctx, userB, out.ID); err != nil || sub == nil {
		t.Fatalf("B must own their own captured lead: %v %v", sub, err)
	}
}
