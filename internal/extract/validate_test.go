package extract

import (
	"testing"
	"time"

	"github.com/EOEboh/sheetdrop/internal/config"
	"github.com/EOEboh/sheetdrop/internal/llm"
)

func strp(s string) *string { return &s }

var day = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func testSchema() config.Schema {
	return config.Schema{
		RequiredFields: []string{"contact", "need"},
		Fields: []config.Field{
			{Name: "name", Type: "string", Nullable: true, Description: "d"},
			{Name: "contact", Type: "string", Nullable: true, Description: "d"},
			{Name: "source", Type: "string", Description: "d"},
			{Name: "need", Type: "string", Description: "d"},
			{Name: "date", Type: "string", Description: "d"},
			{Name: "notes", Type: "string", Description: "d"},
		},
	}
}

func okResult() *llm.Result {
	return &llm.Result{
		Name:       strp("Jane Doe"),
		Contact:    strp("jane@example.com"),
		Source:     "cold email",
		Need:       "Wants a landing page",
		Date:       "2026-07-01",
		Notes:      "budget $500",
		Confidence: "high",
		FieldConfidence: map[string]string{
			"name": "high", "contact": "high", "source": "high",
			"need": "high", "date": "high", "notes": "high",
		},
	}
}

func TestValidatePasses(t *testing.T) {
	v := Validate(okResult(), testSchema(), day)
	if v.NeedsAttention {
		t.Fatalf("expected pass, got needs_attention: %v", v.Reasons)
	}
	for name, st := range v.Fields {
		if st != FieldOK {
			t.Fatalf("field %q = %s, want ok", name, st)
		}
	}
}

func TestLowFieldConfidenceFlagsThatFieldOnly(t *testing.T) {
	r := okResult()
	r.FieldConfidence["contact"] = "low"
	v := Validate(r, testSchema(), day)
	if !v.NeedsAttention {
		t.Fatal("low field confidence should need attention")
	}
	if v.Fields["contact"] != FieldLowConfidence {
		t.Fatalf("contact = %s, want low_confidence", v.Fields["contact"])
	}
	if v.Fields["need"] != FieldOK {
		t.Fatalf("need = %s, want ok", v.Fields["need"])
	}
}

func TestMissingRequiredFieldFlagged(t *testing.T) {
	r := okResult()
	r.Contact = nil
	v := Validate(r, testSchema(), day)
	if !v.NeedsAttention {
		t.Fatal("missing contact should need attention")
	}
	if v.Fields["contact"] != FieldMissing {
		t.Fatalf("contact = %s, want missing", v.Fields["contact"])
	}
}

func TestMissingBeatsLowConfidence(t *testing.T) {
	r := okResult()
	r.Contact = nil
	r.FieldConfidence["contact"] = "low"
	v := Validate(r, testSchema(), day)
	if v.Fields["contact"] != FieldMissing {
		t.Fatalf("contact = %s, want missing to win over low confidence", v.Fields["contact"])
	}
}

func TestNonRequiredAbsentFieldIsOK(t *testing.T) {
	// Absent non-required fields render as plain empty inputs, not flagged.
	r := okResult()
	r.Name = nil
	v := Validate(r, testSchema(), day)
	if v.Fields["name"] != FieldOK {
		t.Fatalf("name = %s, want ok", v.Fields["name"])
	}
	if v.NeedsAttention {
		t.Fatalf("absent non-required field should not need attention: %v", v.Reasons)
	}
}

func TestModelMissingReportIgnoredWhenFieldPresent(t *testing.T) {
	// Models sometimes self-report a field as missing while extracting it
	// fine (observed live with gpt-5.4-nano). Actual presence is
	// authoritative.
	r := okResult()
	r.MissingFields = []string{"need", "name"}
	v := Validate(r, testSchema(), day)
	if v.Fields["need"] != FieldOK {
		t.Fatalf("present field flagged despite model self-report: %v", v.Reasons)
	}
}

func TestNilFieldConfidenceFallsBackToOverall(t *testing.T) {
	// Extractions stored before field_confidence existed: overall "low"
	// taints every present field so old queue rows still show caution.
	r := okResult()
	r.FieldConfidence = nil
	r.Confidence = "low"
	v := Validate(r, testSchema(), day)
	for _, name := range []string{"name", "contact", "source", "need", "date", "notes"} {
		if v.Fields[name] != FieldLowConfidence {
			t.Fatalf("field %q = %s, want low_confidence fallback", name, v.Fields[name])
		}
	}
}

func TestNilFieldConfidenceHighOverallIsOK(t *testing.T) {
	r := okResult()
	r.FieldConfidence = nil
	r.Confidence = "high"
	v := Validate(r, testSchema(), day)
	if v.NeedsAttention {
		t.Fatalf("high overall with nil map should be all ok: %v", v.Reasons)
	}
}

func TestDateDefaultsToSubmissionDate(t *testing.T) {
	r := okResult()
	r.Date = ""
	Validate(r, testSchema(), day)
	if r.Date != "2026-07-08" {
		t.Fatalf("date not defaulted, got %q", r.Date)
	}
}

func TestMultipleLeadsFlagged(t *testing.T) {
	r := okResult()
	r.MultipleLeadsDetected = true
	v := Validate(r, testSchema(), day)
	if v.NeedsAttention {
		t.Fatal("multiple leads alone should not need attention")
	}
	if len(v.Flags) == 0 {
		t.Fatal("expected a multiple-leads flag")
	}
}

func TestJSONSchemaShape(t *testing.T) {
	s := testSchema()
	js := JSONSchema(s)
	if js["additionalProperties"] != false {
		t.Fatal("additionalProperties must be false")
	}
	props := js["properties"].(map[string]any)
	for _, want := range []string{"contact", "need", "confidence", "field_confidence", "missing_fields", "multiple_leads_detected"} {
		if _, ok := props[want]; !ok {
			t.Fatalf("schema missing property %q", want)
		}
	}
	req := js["required"].([]string)
	if len(req) != len(props) {
		t.Fatalf("all properties must be required: %d props, %d required", len(props), len(req))
	}

	// Strict structured outputs: the nested field_confidence object must
	// require all its properties and forbid extras, or the API 400s.
	fc := props["field_confidence"].(map[string]any)
	if fc["additionalProperties"] != false {
		t.Fatal("field_confidence.additionalProperties must be false")
	}
	fcProps := fc["properties"].(map[string]any)
	fcReq := fc["required"].([]string)
	if len(fcProps) != len(s.Fields) || len(fcReq) != len(s.Fields) {
		t.Fatalf("field_confidence must cover all %d fields: %d props, %d required",
			len(s.Fields), len(fcProps), len(fcReq))
	}
}
