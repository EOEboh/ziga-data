package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
