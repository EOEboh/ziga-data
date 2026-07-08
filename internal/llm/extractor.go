// Package llm defines the provider-agnostic extraction interface. Nothing
// outside this package imports a provider SDK, so swapping or adding
// providers only touches this package.
package llm

import (
	"context"
	"time"
)

// Input is one submission to extract a lead from. Text and Image may both be
// set; at least one must be.
type Input struct {
	Text           string
	Image          []byte
	ImageMediaType string // "image/png", "image/jpeg", "image/webp", "image/gif"
	SubmissionDate time.Time
}

// Result is the structured extraction returned by the model.
type Result struct {
	Name                  *string  `json:"name"`
	Contact               *string  `json:"contact"`
	Source                string   `json:"source"`
	Need                  string   `json:"need"`
	Date                  string   `json:"date"`
	Notes                 string   `json:"notes"`
	Confidence            string   `json:"confidence"` // "high" | "medium" | "low"
	MissingFields         []string `json:"missing_fields"`
	MultipleLeadsDetected bool     `json:"multiple_leads_detected"`
}

// Field returns the extracted value for a schema field name, and whether it
// is present (non-null, non-empty). Used by validation and the sheet writer
// so field names stay configuration, not code.
func (r *Result) Field(name string) (string, bool) {
	switch name {
	case "name":
		if r.Name == nil || *r.Name == "" {
			return "", false
		}
		return *r.Name, true
	case "contact":
		if r.Contact == nil || *r.Contact == "" {
			return "", false
		}
		return *r.Contact, true
	case "source":
		return r.Source, r.Source != ""
	case "need":
		return r.Need, r.Need != ""
	case "date":
		return r.Date, r.Date != ""
	case "notes":
		return r.Notes, r.Notes != ""
	default:
		return "", false
	}
}

// Extractor turns an unstructured submission into a structured Result.
type Extractor interface {
	Extract(ctx context.Context, in Input) (*Result, error)
}
