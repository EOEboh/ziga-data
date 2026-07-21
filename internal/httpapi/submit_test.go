package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"time"

	"github.com/EOEboh/ziga-data/internal/auth"
	"github.com/EOEboh/ziga-data/internal/config"
	"github.com/EOEboh/ziga-data/internal/extract"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/mail"
	"github.com/EOEboh/ziga-data/internal/store"
)

type fakeExtractor struct {
	result *llm.Result
	err    error
	calls  int
}

func (f *fakeExtractor) Extract(_ context.Context, _ llm.Input) (*llm.Result, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	cp := *f.result
	return &cp, nil
}

type fakeWriter struct {
	rows [][]string
	err  error
}

func (f *fakeWriter) Append(_ context.Context, row []string) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeWriter) LastRows(_ context.Context, n int) ([][]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	rows := f.rows
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows, nil
}

func strp(s string) *string { return &s }

func goodResult() *llm.Result {
	return &llm.Result{
		Name: strp("Jane"), Contact: strp("jane@x.com"),
		Source: "X DM", Need: "logo design", Date: "2026-07-08",
		Notes: "budget $500", Confidence: "high",
		FieldConfidence: map[string]string{
			"name": "high", "contact": "high", "source": "high",
			"need": "high", "date": "high", "notes": "high",
		},
	}
}

func testServer(t *testing.T, ex llm.Extractor, w RowWriter) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		RatePerMin:    1000,
		SheetTab:      "Leads",
		SessionSecret: "test-secret",
		AppBaseURL:    "http://localhost:8080",
		Schema: config.Schema{
			RequiredFields: []string{"contact", "need"},
			Fields: []config.Field{
				{Name: "name"}, {Name: "contact"}, {Name: "source"},
				{Name: "need"}, {Name: "date"}, {Name: "notes"},
			},
			Columns: []string{"date", "name", "contact", "source", "need", "notes", "flags"},
		},
	}
	return New(cfg, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)), ex, st, w, &mail.FakeMailer{})
}

const testUserEmail = "user@test.example"

// handler builds the route tree and wraps it so every request carries a valid
// session and CSRF token for a single seeded, verified test user — the tenant
// that testUID resolves to. This lets the submission tests exercise the real
// requireAuth + CSRF middleware without each test re-plumbing auth.
func handler(s *Server) http.Handler {
	real := s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}})
	ctx := context.Background()
	u, err := s.store.GetUserByEmail(ctx, testUserEmail)
	if err != nil {
		if u, err = s.store.CreateUser(ctx, testUserEmail, ""); err != nil {
			panic(err)
		}
		s.store.MarkEmailVerified(ctx, u.ID)
	}
	token, _ := auth.RandomToken()
	s.store.CreateSession(ctx, auth.HashToken(token), u.ID, time.Now().Add(time.Hour))
	csrf, _ := auth.NewCSRFToken(s.sessionSecret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(sessionCookie); err != nil {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		if _, err := r.Cookie(csrfCookie); err != nil {
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
			r.Header.Set("X-CSRF-Token", csrf)
		}
		real.ServeHTTP(w, r)
	})
}

// testUID returns the id of the seeded test user, so store assertions scope to
// the same tenant the handlers wrote as.
func testUID(t *testing.T, s *Server) int64 {
	t.Helper()
	u, err := s.store.GetUserByEmail(context.Background(), testUserEmail)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func postText(t *testing.T, h http.Handler, text string) (*httptest.ResponseRecorder, submissionResponse) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if text != "" {
		mw.WriteField("text", text)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/submit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp submissionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func postConfirm(t *testing.T, h http.Handler, id int64, fields map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"fields": fields})
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/submissions/%d/confirm", id), bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestSubmitExtractsWithoutWriting(t *testing.T) {
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	rec, resp := postText(t, handler(s), "Jane wants a logo, jane@x.com")
	if rec.Code != 200 || resp.Status != store.StatusPending {
		t.Fatalf("code=%d status=%s", rec.Code, resp.Status)
	}
	if len(w.rows) != 0 {
		t.Fatal("submit must not write to the sheet")
	}
	if resp.ID == 0 {
		t.Fatal("response must carry the submission id")
	}
	for name, st := range resp.FieldStates {
		if st != extract.FieldOK {
			t.Fatalf("field %q = %s, want ok", name, st)
		}
	}
	if resp.Input.Text == "" {
		t.Fatal("response must echo the original input")
	}
}

func TestSubmitFlagsLowConfidenceAndMissing(t *testing.T) {
	r := goodResult()
	r.Contact = nil
	r.FieldConfidence["need"] = "low"
	s := testServer(t, &fakeExtractor{result: r}, &fakeWriter{})
	_, resp := postText(t, handler(s), "someone wants something")
	if resp.Status != store.StatusPending {
		t.Fatalf("status=%s", resp.Status)
	}
	if resp.FieldStates["contact"] != extract.FieldMissing {
		t.Fatalf("contact = %s, want missing", resp.FieldStates["contact"])
	}
	if resp.FieldStates["need"] != extract.FieldLowConfidence {
		t.Fatalf("need = %s, want low_confidence", resp.FieldStates["need"])
	}
}

func TestConfirmWritesEditedRow(t *testing.T) {
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")

	rec, resp := postConfirm(t, h, sub.ID, map[string]string{"name": "Jane Doe", "notes": ""})
	if rec.Code != 200 || resp["status"] != string(store.StatusWritten) {
		t.Fatalf("code=%d resp=%v", rec.Code, resp)
	}
	if len(w.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(w.rows))
	}
	want := []string{"2026-07-08", "Jane Doe", "jane@x.com", "X DM", "logo design", "", ""}
	for i, v := range want {
		if w.rows[0][i] != v {
			t.Fatalf("row[%d] = %q, want %q (row: %v)", i, w.rows[0][i], v, w.rows[0])
		}
	}
	// The edited extraction is persisted.
	stored, _ := s.store.Get(context.Background(), testUID(t, s), sub.ID)
	if stored.Status != store.StatusWritten || !strings.Contains(string(stored.Extraction), "Jane Doe") {
		t.Fatalf("stored: status=%s extraction=%s", stored.Status, stored.Extraction)
	}
}

func TestConfirmTwiceIsConflict(t *testing.T) {
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")
	postConfirm(t, h, sub.ID, nil)
	rec, _ := postConfirm(t, h, sub.ID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d, want 409", rec.Code)
	}
	if len(w.rows) != 1 {
		t.Fatalf("second confirm must not write again: %d rows", len(w.rows))
	}
}

func TestConfirmMissingRequiredRejected(t *testing.T) {
	r := goodResult()
	r.Contact = nil
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: r}, w)
	h := handler(s)
	_, sub := postText(t, h, "someone wants something")

	rec, resp := postConfirm(t, h, sub.ID, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d, want 422", rec.Code)
	}
	if len(w.rows) != 0 {
		t.Fatal("nothing may be written with a required field missing")
	}
	states := resp["field_states"].(map[string]any)
	if states["contact"] != string(extract.FieldMissing) {
		t.Fatalf("field_states = %v", states)
	}
	// Filling the field via the edit payload makes the same confirm pass.
	rec, _ = postConfirm(t, h, sub.ID, map[string]string{"contact": "jane@x.com"})
	if rec.Code != 200 || len(w.rows) != 1 {
		t.Fatalf("code=%d rows=%d after filling required field", rec.Code, len(w.rows))
	}
}

func TestConfirmFailureKeepsDataAndRetries(t *testing.T) {
	w := &fakeWriter{err: errors.New("boom")}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")

	rec, resp := postConfirm(t, h, sub.ID, map[string]string{"name": "Edited"})
	if rec.Code != http.StatusBadGateway || resp["status"] != string(store.StatusFailedWrite) {
		t.Fatalf("code=%d resp=%v", rec.Code, resp)
	}
	stored, _ := s.store.Get(context.Background(), testUID(t, s), sub.ID)
	if stored.Status != store.StatusFailedWrite {
		t.Fatalf("status=%s, want failed_write", stored.Status)
	}
	if !strings.Contains(string(stored.Extraction), "Edited") {
		t.Fatal("edited data must survive a failed write")
	}
	// Retry is the same call once the writer recovers.
	w.err = nil
	rec, _ = postConfirm(t, h, sub.ID, map[string]string{"name": "Edited"})
	if rec.Code != 200 || len(w.rows) != 1 {
		t.Fatalf("retry: code=%d rows=%d", rec.Code, len(w.rows))
	}
}

func TestConfirmUnknownSubmission(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	rec, _ := postConfirm(t, handler(s), 999, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404", rec.Code)
	}
}

func TestDiscardFreesSubmission(t *testing.T) {
	ex := &fakeExtractor{result: goodResult()}
	s := testServer(t, ex, &fakeWriter{})
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/submissions/%d/discard", sub.ID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("discard code=%d", rec.Code)
	}
	// Soft delete: the row is retained but leaves the queue and frees the
	// dedup hash for genuine resubmission.
	kept, _ := s.store.Get(context.Background(), testUID(t, s), sub.ID)
	if kept == nil || kept.Status != store.StatusDiscarded {
		t.Fatalf("discarded submission must be retained with status discarded, got %+v", kept)
	}
	_, resub := postText(t, h, "Jane wants a logo")
	if resub.Duplicate || resub.ID == sub.ID {
		t.Fatalf("resubmit after discard must create a new submission: %+v", resub)
	}
	if ex.calls != 2 {
		t.Fatalf("resubmit must re-extract, calls=%d", ex.calls)
	}

	// A second discard is idempotent.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", fmt.Sprintf("/api/submissions/%d/discard", sub.ID), nil))
	if rec2.Code != 200 {
		t.Fatalf("second discard code=%d", rec2.Code)
	}
}

// TestSubmitAndConfirmShareRateLimit proves both endpoints draw from one
// per-IP budget: with RATE_LIMIT_PER_MIN=1 (burst 5, negligible refill),
// five requests in any mix exhaust it and the sixth 429s on either endpoint.
func TestSubmitAndConfirmShareRateLimit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		RatePerMin:    1,
		SheetTab:      "Leads",
		SessionSecret: "test-secret",
		AppBaseURL:    "http://localhost:8080",
		Schema: config.Schema{
			RequiredFields: []string{"contact", "need"},
			Fields: []config.Field{
				{Name: "name"}, {Name: "contact"}, {Name: "source"},
				{Name: "need"}, {Name: "date"}, {Name: "notes"},
			},
			Columns: []string{"date", "name", "contact", "source", "need", "notes", "flags"},
		},
	}
	s := New(cfg, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)), &fakeExtractor{result: goodResult()}, st, &fakeWriter{}, &mail.FakeMailer{})
	h := handler(s)

	rec, sub := postText(t, h, "Jane wants a logo, jane@x.com") // token 1
	if rec.Code != 200 {
		t.Fatalf("submit code=%d", rec.Code)
	}
	for i := 0; i < 4; i++ { // tokens 2-5; app-level statuses (200 then 409) are fine
		if crec, _ := postConfirm(t, h, sub.ID, nil); crec.Code == 429 {
			t.Fatalf("confirm %d hit the limit early", i)
		}
	}
	if crec, _ := postConfirm(t, h, sub.ID, nil); crec.Code != 429 {
		t.Fatalf("6th request (confirm) code=%d, want 429", crec.Code)
	}
	if srec, _ := postText(t, h, "another lead"); srec.Code != 429 {
		t.Fatalf("7th request (submit) code=%d, want 429 from the shared budget", srec.Code)
	}
}

func TestConfirmAfterDiscardRejected(t *testing.T) {
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", fmt.Sprintf("/api/submissions/%d/discard", sub.ID), nil))
	if rec.Code != 200 {
		t.Fatalf("discard code=%d", rec.Code)
	}
	crec, _ := postConfirm(t, h, sub.ID, nil)
	if crec.Code != 409 {
		t.Fatalf("confirm after discard code=%d, want 409", crec.Code)
	}
	if len(w.rows) != 0 {
		t.Fatal("discarded submission must never reach the sheet")
	}
}

func TestQueueListsPendingAndFailed(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{err: errors.New("boom")})
	h := handler(s)
	_, sub := postText(t, h, "lead one")
	postConfirm(t, h, sub.ID, nil) // becomes failed_write

	req := httptest.NewRequest("GET", "/api/queue", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp struct {
		Count int                  `json:"count"`
		Items []submissionResponse `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Count != 1 || len(resp.Items) != 1 {
		t.Fatalf("count=%d items=%d", resp.Count, len(resp.Items))
	}
	if resp.Items[0].Status != store.StatusFailedWrite || resp.Items[0].Error == "" {
		t.Fatalf("item: %+v", resp.Items[0])
	}
}

func TestPreviewPadsRows(t *testing.T) {
	w := &fakeWriter{rows: [][]string{{"2026-07-01", "Ada"}}}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	req := httptest.NewRequest("GET", "/api/preview", nil)
	rec := httptest.NewRecorder()
	handler(s).ServeHTTP(rec, req)
	var resp struct {
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Rows) != 1 || len(resp.Rows[0]) != len(resp.Columns) {
		t.Fatalf("rows not padded to columns: %v vs %v", resp.Rows, resp.Columns)
	}
}

func TestPreviewDegradesOnError(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{err: errors.New("boom")})
	req := httptest.NewRequest("GET", "/api/preview", nil)
	rec := httptest.NewRecorder()
	handler(s).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("preview must degrade with 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "preview unavailable") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestDuplicateSubmissionNoSecondCall(t *testing.T) {
	ex := &fakeExtractor{result: goodResult()}
	s := testServer(t, ex, &fakeWriter{})
	h := handler(s)
	postText(t, h, "same lead text")
	_, resp := postText(t, h, "same lead text")
	if !resp.Duplicate {
		t.Fatal("second submit should be marked duplicate")
	}
	if ex.calls != 1 {
		t.Fatalf("extractor called %d times, want 1", ex.calls)
	}
}

func TestEmptySubmissionRejected(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	rec, _ := postText(t, handler(s), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d, want 400", rec.Code)
	}
}

func TestMultipleLeadsFlagSurfaces(t *testing.T) {
	r := goodResult()
	r.MultipleLeadsDetected = true
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: r}, w)
	h := handler(s)
	_, resp := postText(t, h, "two leads in one paste")
	if len(resp.Flags) == 0 {
		t.Fatal("expected multiple-leads flag in response")
	}
	if resp.Result == nil || !resp.Result.MultipleLeadsDetected {
		t.Fatal("multi-lead bit must surface on the result")
	}
	// The flag rides along to the sheet on confirm.
	postConfirm(t, h, resp.ID, nil)
	if len(w.rows) != 1 || w.rows[0][6] == "" {
		t.Fatalf("expected flags column populated: %v", w.rows)
	}
}

func TestDestinationListsSheetAndNotion(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	req := httptest.NewRequest("GET", "/api/destination", nil)
	rec := httptest.NewRecorder()
	handler(s).ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Leads (Google Sheet)") || !strings.Contains(body, "Notion") {
		t.Fatalf("body: %s", body)
	}
}

func TestHistoryListsWritten(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{})
	h := handler(s)
	_, sub := postText(t, h, "Jane wants a logo")
	postConfirm(t, h, sub.ID, nil)

	req := httptest.NewRequest("GET", "/api/history", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 1 {
		t.Fatalf("history items = %d, want 1", len(resp.Items))
	}
}
