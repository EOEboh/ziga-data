package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/EOEboh/ziga-data/internal/extract"
	"github.com/EOEboh/ziga-data/internal/llm"
	"github.com/EOEboh/ziga-data/internal/store"
)

// Failure modes of ingestLead, so each caller maps them onto its own status
// codes: a browser paste and an email webhook report the same fault
// differently.
var (
	errDedupLookup      = errors.New("dedup lookup failed")
	errExtractionFailed = errors.New("extraction failed")
	errStoreInsert      = errors.New("store insert failed")
)

// leadInput is one piece of raw lead material to process, from any channel.
// The email fields are zero on the paste path.
type leadInput struct {
	UserID    int64
	Text      string
	Image     []byte
	ImageType string
	Now       time.Time

	Source      store.Source
	MessageID   string
	FromAddress string
	Subject     string
	ReceivedAt  time.Time
	// Email carries envelope context into the extraction prompt, so the model
	// knows a forwarded message's lead is the original sender rather than
	// whoever forwarded it. Nil for pastes.
	Email *llm.EmailMeta
	// ExtraFlags are review-pane notices the caller determined before
	// extraction (a truncated body, a low-confidence sender attribution).
	// They are merged with the validator's own flags.
	ExtraFlags []string
	// ReplacesID is the submission this one supersedes, set when the user
	// re-runs an extraction with corrected text. It suppresses the Message-ID
	// check: the only row that check could match is the one being replaced,
	// which is still present because it is not discarded until the new
	// extraction has actually succeeded.
	ReplacesID int64
}

// leadOutcome is what ingestLead did.
type leadOutcome struct {
	Submission *store.Submission
	Duplicate  bool
	Verdict    extract.Verdict
}

// ingestLead is the one path from raw lead material to a stored pending
// submission: dedup, extract, validate, insert. Both POST /api/submit and POST
// /api/ingest/email go through it.
//
// It is the only call site of the extractor in this package, which
// TestExtractorSingleCallSite enforces. That matters because the email path's
// filter pipeline is a cost control as much as a quality one: a second door
// into the model would be a way for unfiltered mail to reach it, and the
// guarantee is only worth anything if there is exactly one door.
//
// Nothing here writes to a destination. A lead becomes a pending submission
// and waits for a human, on every channel.
func (s *Server) ingestLead(ctx context.Context, in leadInput) (*leadOutcome, error) {
	if in.Source == "" {
		in.Source = store.SourcePaste
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}

	// Message-ID dedup runs before the content hash because it catches what
	// the hash cannot: the hash buckets by calendar day, so a mail system
	// redelivering the same message across midnight UTC hashes differently and
	// would insert a second copy of one lead.
	if in.MessageID != "" && in.ReplacesID == 0 {
		since := in.Now.Add(-messageIDDedupWindow)
		prior, err := s.store.FindByMessageID(ctx, in.UserID, in.MessageID, since)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errDedupLookup, err)
		}
		if prior != nil {
			return &leadOutcome{Submission: prior, Duplicate: true}, nil
		}
	}

	hash := store.ContentHash(in.UserID, in.Text, in.Image, in.Now)

	// Idempotency: identical content from the same user today returns the
	// prior outcome without another (billable) extraction.
	prior, err := s.store.FindByHash(ctx, in.UserID, hash)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDedupLookup, err)
	}
	if prior != nil {
		return &leadOutcome{Submission: prior, Duplicate: true}, nil
	}

	result, err := s.extractor.Extract(ctx, llm.Input{
		Text:           in.Text,
		Image:          in.Image,
		ImageMediaType: in.ImageType,
		SubmissionDate: in.Now,
		Email:          in.Email,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errExtractionFailed, err)
	}

	verdict := extract.Validate(result, s.cfg.Schema, in.Now)
	flags := append(append([]string{}, in.ExtraFlags...), verdict.Flags...)
	resultJSON, _ := json.Marshal(result)
	flagsJSON, _ := json.Marshal(flags)

	sub := &store.Submission{
		UserID:         in.UserID,
		ContentHash:    hash,
		Status:         store.StatusPending,
		Extraction:     resultJSON,
		Flags:          flagsJSON,
		InputExcerpt:   excerpt(in.Text, in.Image),
		InputText:      in.Text,
		InputImage:     in.Image,
		InputImageType: in.ImageType,
		Source:         in.Source,
		MessageID:      in.MessageID,
		FromAddress:    in.FromAddress,
		Subject:        in.Subject,
		ReceivedAt:     in.ReceivedAt,
	}
	duplicate, err := s.store.Insert(ctx, sub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errStoreInsert, err)
	}
	if duplicate {
		// Lost an insert race with an identical concurrent submission.
		prior, err := s.store.FindByHash(ctx, in.UserID, hash)
		if err != nil || prior == nil {
			return nil, fmt.Errorf("%w: lost insert race and could not re-read", errStoreInsert)
		}
		return &leadOutcome{Submission: prior, Duplicate: true}, nil
	}

	return &leadOutcome{Submission: sub, Verdict: verdict}, nil
}

// messageIDDedupWindow bounds how far back a Message-ID lookup reaches. Mail
// systems retry for hours, not weeks, and an unbounded window would block a
// user who deliberately re-forwards an old lead after cleaning up their queue.
const messageIDDedupWindow = 7 * 24 * time.Hour
