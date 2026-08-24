package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/EOEboh/ziga-data/internal/store"
)

var errTestExtraction = errors.New("model unavailable")

func postRerun(t *testing.T, h http.Handler, id int64, text string) (*httptest.ResponseRecorder, submissionResponse) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("text", text)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/submissions/"+strconv.FormatInt(id, 10)+"/rerun", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out submissionResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestRerunKeepsEmailProvenance is the regression test for the bug this
// endpoint exists to fix.
//
// Re-running used to mean "submit the edited text as a new paste, then discard
// the original", which silently converted an email-captured lead into a pasted
// one. For a forwarded message that destroys the sender address — the only
// record of who the lead actually is, and the entire output of the attribution
// work in internal/ingest. One click, unrecoverable from the UI.
func TestRerunKeepsEmailProvenance(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	// Capture a lead by email, the normal way.
	_, ingested := postIngest(t, rawHandler(s), mailTo(addr))
	if ingested.Status != ingestAccepted {
		t.Fatalf("setup: %+v", ingested)
	}
	before, err := s.store.Get(ctx, uid, ingested.ID)
	if err != nil || before == nil {
		t.Fatal(err)
	}

	rec, out := postRerun(t, h, ingested.ID, "Corrected: Ada needs a landing page by March, ada@lumen.studio")
	if rec.Code != http.StatusOK {
		t.Fatalf("rerun: %d %s", rec.Code, rec.Body)
	}
	if out.ID == ingested.ID {
		t.Fatal("edited text should produce a replacement, not the original row")
	}

	after, err := s.store.Get(ctx, uid, out.ID)
	if err != nil || after == nil {
		t.Fatal(err)
	}
	if after.Source != store.SourceEmail {
		t.Errorf("source = %q, want email — a re-run must not turn a captured lead into a paste", after.Source)
	}
	if after.FromAddress != before.FromAddress {
		t.Errorf("from = %q, want %q preserved; for a forwarded lead this is the only record of who they are",
			after.FromAddress, before.FromAddress)
	}
	if after.Subject != before.Subject {
		t.Errorf("subject = %q, want %q preserved", after.Subject, before.Subject)
	}
	if !after.ReceivedAt.Equal(before.ReceivedAt) {
		t.Errorf("received_at = %v, want %v preserved", after.ReceivedAt, before.ReceivedAt)
	}
	// And the response the UI renders carries it, so the badge survives too.
	if out.Source != store.SourceEmail || out.FromAddress == "" {
		t.Errorf("response lacks provenance, so the Email badge would vanish: %+v", out)
	}

	// The original is replaced, not left behind as a duplicate.
	orig, err := s.store.Get(ctx, uid, ingested.ID)
	if err != nil || orig == nil {
		t.Fatal(err)
	}
	if orig.Status != store.StatusDiscarded {
		t.Errorf("original status = %q, want discarded", orig.Status)
	}
}

// TestRerunCannotForgeProvenance: provenance is read from the stored row, never
// from the request. A client able to assert from_address could stamp any paste
// as having come from someone it did not.
func TestRerunCannotForgeProvenance(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)

	// A plain pasted lead.
	pasted := &store.Submission{
		UserID: uid, ContentHash: "paste-1", Status: store.StatusPending,
		Source: store.SourcePaste, InputText: "walk-in enquiry",
	}
	if _, err := s.store.Insert(ctx, pasted); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("text", "an edited walk-in enquiry with more detail added")
	// These must be ignored outright.
	mw.WriteField("source", "email")
	mw.WriteField("from_address", "ceo@bigcorp.example.com")
	mw.WriteField("subject", "forged")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost,
		"/api/submissions/"+strconv.FormatInt(pasted.ID, 10)+"/rerun", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rerun: %d %s", rec.Code, rec.Body)
	}
	var out submissionResponse
	json.Unmarshal(rec.Body.Bytes(), &out)

	got, err := s.store.Get(ctx, uid, out.ID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Source != store.SourcePaste {
		t.Errorf("source = %q — the client talked the server into changing the channel", got.Source)
	}
	if got.FromAddress != "" {
		t.Errorf("from_address = %q — a paste was stamped with a forged sender", got.FromAddress)
	}
}

func TestRerunIsolatedAndGuarded(t *testing.T) {
	a := newAuthTest(t)
	enableIngestion(t, a)
	ctx := context.Background()
	userA := mustVerifiedUser(t, a, "rr-a@x.com")
	userB := mustVerifiedUser(t, a, "rr-b@x.com")
	sessB := mustSession(t, a, userB)
	csrf := a.cookies[csrfCookie]

	subA := &store.Submission{UserID: userA, ContentHash: "rr-a", Status: store.StatusPending, InputText: "A's lead"}
	if _, err := a.s.store.Insert(ctx, subA); err != nil {
		t.Fatal(err)
	}

	// Cross-tenant re-run is 404 (not 403 — ids stay non-enumerable) and must
	// not touch A's row.
	rec := a.reqAs(sessB, csrf, "POST", "/api/submissions/"+itoa(subA.ID)+"/rerun", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant rerun: %d, want 404", rec.Code)
	}
	still, _ := a.s.store.Get(ctx, userA, subA.ID)
	if still == nil || still.Status != store.StatusPending {
		t.Fatalf("A's submission was disturbed: %+v", still)
	}

	// An already-written lead cannot be re-run: it would invite a duplicate row
	// at the destination.
	written := &store.Submission{UserID: userA, ContentHash: "rr-w", Status: store.StatusWritten, InputText: "done"}
	if _, err := a.s.store.Insert(ctx, written); err != nil {
		t.Fatal(err)
	}
	sessA := mustSession(t, a, userA)
	rec = a.reqAs(sessA, csrf, "POST", "/api/submissions/"+itoa(written.ID)+"/rerun", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rerun of a written lead: %d, want 409", rec.Code)
	}
}

// TestRerunFailureLeavesTheOriginal: the original is discarded only once a
// replacement exists, so a transient model outage cannot cost the user the
// lead they were trying to correct.
func TestRerunFailureLeavesTheOriginal(t *testing.T) {
	s, _ := ingestServer(t)
	h := handler(s)
	ctx := context.Background()
	uid := testUID(t, s)
	addr := enableInbound(t, s, uid)

	_, ingested := postIngest(t, rawHandler(s), mailTo(addr))
	s.extractor = &fakeExtractor{err: errTestExtraction}

	rec, _ := postRerun(t, h, ingested.ID, "edited text that will fail to extract")
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error", rec.Code)
	}
	orig, err := s.store.Get(ctx, uid, ingested.ID)
	if err != nil || orig == nil {
		t.Fatal(err)
	}
	if orig.Status != store.StatusPending {
		t.Fatalf("original status = %q — a failed re-run destroyed the lead being corrected", orig.Status)
	}
}
