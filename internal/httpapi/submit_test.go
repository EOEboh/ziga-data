package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/EOEboh/sheetdrop/internal/config"
	"github.com/EOEboh/sheetdrop/internal/llm"
	"github.com/EOEboh/sheetdrop/internal/store"
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

func strp(s string) *string { return &s }

func goodResult() *llm.Result {
	return &llm.Result{
		Name: strp("Jane"), Contact: strp("jane@x.com"),
		Source: "X DM", Need: "logo design", Date: "2026-07-08",
		Notes: "budget $500", Confidence: "high",
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
		RatePerMin: 1000,
		Schema: config.Schema{
			RequiredFields: []string{"contact", "need"},
			Columns:        []string{"date", "name", "contact", "source", "need", "notes", "flags"},
		},
	}
	return New(cfg, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)), ex, st, w)
}

func postText(t *testing.T, h http.Handler, text string) (*httptest.ResponseRecorder, submitResponse) {
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
	var resp submitResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func handler(s *Server) http.Handler {
	return s.Handler(fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")}})
}

func TestSubmitWritesRow(t *testing.T) {
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: goodResult()}, w)
	rec, resp := postText(t, handler(s), "Jane wants a logo, jane@x.com")
	if rec.Code != 200 || resp.Status != store.StatusWritten {
		t.Fatalf("code=%d status=%s", rec.Code, resp.Status)
	}
	if len(w.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(w.rows))
	}
	want := []string{"2026-07-08", "Jane", "jane@x.com", "X DM", "logo design", "budget $500", ""}
	for i, v := range want {
		if w.rows[0][i] != v {
			t.Fatalf("row[%d] = %q, want %q (row: %v)", i, w.rows[0][i], v, w.rows[0])
		}
	}
}

func TestLowConfidenceGoesToReview(t *testing.T) {
	r := goodResult()
	r.Confidence = "low"
	w := &fakeWriter{}
	s := testServer(t, &fakeExtractor{result: r}, w)
	_, resp := postText(t, handler(s), "blurry stuff")
	if resp.Status != store.StatusNeedsReview {
		t.Fatalf("status=%s", resp.Status)
	}
	if len(w.rows) != 0 {
		t.Fatal("needs_review must not write to the sheet")
	}
}

func TestMissingRequiredGoesToReview(t *testing.T) {
	r := goodResult()
	r.Contact = nil
	s := testServer(t, &fakeExtractor{result: r}, &fakeWriter{})
	_, resp := postText(t, handler(s), "someone wants something")
	if resp.Status != store.StatusNeedsReview {
		t.Fatalf("status=%s", resp.Status)
	}
}

func TestWriterFailureRecorded(t *testing.T) {
	s := testServer(t, &fakeExtractor{result: goodResult()}, &fakeWriter{err: errors.New("boom")})
	_, resp := postText(t, handler(s), "Jane wants a logo")
	if resp.Status != store.StatusFailedWrite {
		t.Fatalf("status=%s", resp.Status)
	}
	failed, err := s.store.ListByStatus(context.Background(), store.StatusFailedWrite, 10)
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed queue: %d err=%v", len(failed), err)
	}
}

func TestDuplicateSubmissionNoSecondCall(t *testing.T) {
	ex := &fakeExtractor{result: goodResult()}
	w := &fakeWriter{}
	s := testServer(t, ex, w)
	h := handler(s)
	postText(t, h, "same lead text")
	_, resp := postText(t, h, "same lead text")
	if !resp.Duplicate {
		t.Fatal("second submit should be marked duplicate")
	}
	if ex.calls != 1 {
		t.Fatalf("extractor called %d times, want 1", ex.calls)
	}
	if len(w.rows) != 1 {
		t.Fatalf("sheet rows = %d, want 1", len(w.rows))
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
	_, resp := postText(t, handler(s), "two leads in one paste")
	if resp.Status != store.StatusWritten {
		t.Fatalf("status=%s", resp.Status)
	}
	if len(resp.Flags) == 0 {
		t.Fatal("expected multiple-leads flag in response")
	}
	if w.rows[0][6] == "" {
		t.Fatal("expected flags column to be populated")
	}
}
