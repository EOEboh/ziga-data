package extract

import (
	"fmt"
	"time"

	"github.com/EOEboh/ziga/internal/config"
	"github.com/EOEboh/ziga/internal/llm"
)

// FieldState is the per-field review state the UI renders.
type FieldState string

const (
	FieldOK            FieldState = "ok"             // plain input
	FieldLowConfidence FieldState = "low_confidence" // amber tint + pill
	FieldMissing       FieldState = "missing"        // required + absent: red border + pill
)

// Verdict is the outcome of gating an extraction result.
type Verdict struct {
	// NeedsAttention is true when any field is not ok — the user should look
	// before confirming. Nothing is written to the sheet without a confirm
	// either way.
	NeedsAttention bool
	// Reasons explain to the user which fields were flagged and why.
	Reasons []string
	// Flags are non-blocking notices (e.g. multiple leads detected).
	Flags []string
	// Fields maps every configured field name to its review state.
	Fields map[string]FieldState
}

// Validate derives per-field review states and normalizes the result in
// place: an empty date is defaulted to the submission date.
func Validate(res *llm.Result, schema config.Schema, submitted time.Time) Verdict {
	v := Verdict{Fields: make(map[string]FieldState, len(schema.Fields))}

	if res.Date == "" {
		res.Date = submitted.Format("2006-01-02")
	}

	required := make(map[string]bool, len(schema.RequiredFields))
	for _, f := range schema.RequiredFields {
		required[f] = true
	}

	// Actual field presence is authoritative: models sometimes list a field
	// in missing_fields while extracting it fine, and a present required
	// field must not be flagged as missing on a stale self-report.
	for _, f := range schema.Fields {
		_, present := res.Field(f.Name)
		switch {
		case required[f.Name] && !present:
			v.Fields[f.Name] = FieldMissing
			v.NeedsAttention = true
			v.Reasons = append(v.Reasons, fmt.Sprintf("required field %q not found", f.Name))
		case present && fieldConfidence(res, f.Name) == "low":
			v.Fields[f.Name] = FieldLowConfidence
			v.NeedsAttention = true
			v.Reasons = append(v.Reasons, fmt.Sprintf("low confidence in %q", f.Name))
		default:
			v.Fields[f.Name] = FieldOK
		}
	}

	if res.MultipleLeadsDetected {
		v.Flags = append(v.Flags, "multiple leads detected — only the primary lead was extracted")
	}
	return v
}

// fieldConfidence reads the per-field confidence, falling back for results
// stored before field_confidence existed: an overall "low" taints every
// field, so old queue rows still render with a caution state.
func fieldConfidence(res *llm.Result, name string) string {
	if res.FieldConfidence != nil {
		return res.FieldConfidence[name]
	}
	return res.Confidence
}
