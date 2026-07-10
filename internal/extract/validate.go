package extract

import (
	"fmt"
	"time"

	"github.com/EOEboh/sheetdrop/internal/llm"
)

// Verdict is the outcome of gating an extraction result.
type Verdict struct {
	NeedsReview bool
	// Reasons explain to the user why the result was flagged for review.
	Reasons []string
	// Flags are non-blocking notices (e.g. multiple leads detected).
	Flags []string
}

// Validate applies the confidence gate and normalizes the result in place:
// low confidence or a missing required field blocks the sheet write; an empty
// date is defaulted to the submission date.
func Validate(res *llm.Result, requiredFields []string, submitted time.Time) Verdict {
	var v Verdict

	if res.Date == "" {
		res.Date = submitted.Format("2006-01-02")
	}

	// Actual field presence is authoritative: models sometimes list a field
	// in missing_fields while extracting it fine, and a present required
	// field must not block the write on a stale self-report. The self-report
	// still catches nothing presence can't — an empty required field blocks
	// either way.
	for _, f := range requiredFields {
		if _, ok := res.Field(f); !ok {
			v.NeedsReview = true
			v.Reasons = append(v.Reasons, fmt.Sprintf("required field %q not found", f))
		}
	}

	if res.Confidence == "low" {
		v.NeedsReview = true
		v.Reasons = append(v.Reasons, "extraction confidence is low")
	}

	if res.MultipleLeadsDetected {
		v.Flags = append(v.Flags, "multiple leads detected — only the primary lead was extracted")
	}
	return v
}
