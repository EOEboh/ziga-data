package extract

import (
	"testing"
	"time"

	"github.com/EOEboh/sheetdrop/internal/config"
	"github.com/EOEboh/sheetdrop/internal/llm"
)

func strp(s string) *string { return &s }

var required = []string{"contact", "need"}
var day = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func okResult() *llm.Result {
	return &llm.Result{
		Name:       strp("Jane Doe"),
		Contact:    strp("jane@example.com"),
		Source:     "cold email",
		Need:       "Wants a landing page",
		Date:       "2026-07-01",
		Confidence: "high",
	}
}

func TestValidatePasses(t *testing.T) {
	v := Validate(okResult(), required, day)
	if v.NeedsReview {
		t.Fatalf("expected pass, got needs_review: %v", v.Reasons)
	}
}

func TestLowConfidenceBlocks(t *testing.T) {
	r := okResult()
	r.Confidence = "low"
	if v := Validate(r, required, day); !v.NeedsReview {
		t.Fatal("low confidence should need review")
	}
}

func TestMissingRequiredFieldBlocks(t *testing.T) {
	r := okResult()
	r.Contact = nil
	v := Validate(r, required, day)
	if !v.NeedsReview {
		t.Fatal("missing contact should need review")
	}
}

func TestModelReportedMissingFieldBlocks(t *testing.T) {
	// Model says a required field is missing even though heuristics can't tell.
	r := okResult()
	r.MissingFields = []string{"need"}
	if v := Validate(r, required, day); !v.NeedsReview {
		t.Fatal("model-reported missing required field should need review")
	}
}

func TestNonRequiredMissingFieldDoesNotBlock(t *testing.T) {
	r := okResult()
	r.Name = nil
	r.MissingFields = []string{"name"}
	if v := Validate(r, required, day); v.NeedsReview {
		t.Fatalf("missing non-required field should not block: %v", v.Reasons)
	}
}

func TestDateDefaultsToSubmissionDate(t *testing.T) {
	r := okResult()
	r.Date = ""
	Validate(r, required, day)
	if r.Date != "2026-07-08" {
		t.Fatalf("date not defaulted, got %q", r.Date)
	}
}

func TestMultipleLeadsFlagged(t *testing.T) {
	r := okResult()
	r.MultipleLeadsDetected = true
	v := Validate(r, required, day)
	if v.NeedsReview {
		t.Fatal("multiple leads alone should not block")
	}
	if len(v.Flags) == 0 {
		t.Fatal("expected a multiple-leads flag")
	}
}

func TestJSONSchemaShape(t *testing.T) {
	s := config.Schema{
		RequiredFields: required,
		Fields: []config.Field{
			{Name: "contact", Type: "string", Nullable: true, Description: "d"},
			{Name: "need", Type: "string", Description: "d"},
		},
	}
	js := JSONSchema(s)
	if js["additionalProperties"] != false {
		t.Fatal("additionalProperties must be false")
	}
	props := js["properties"].(map[string]any)
	for _, want := range []string{"contact", "need", "confidence", "missing_fields", "multiple_leads_detected"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("schema missing property %q", want)
		}
	}
	req := js["required"].([]string)
	if len(req) != len(props) {
		t.Fatalf("all properties must be required: %d props, %d required", len(props), len(req))
	}
}
