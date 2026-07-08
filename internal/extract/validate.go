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

	// Trust actual field presence over the model's self-report, but surface
	// both: a required field that is empty is missing even if the model
	// forgot to list it, and vice versa.
	missing := map[string]bool{}
	for _, f := range res.MissingFields {
		missing[f] = true
	}
	for _, f := range requiredFields {
		if _, ok := res.Field(f); !ok {
			missing[f] = true
		}
	}
	for _, f := range requiredFields {
		if missing[f] {
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
