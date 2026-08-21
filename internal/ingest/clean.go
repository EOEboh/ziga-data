package ingest

import (
	"regexp"
	"strings"
)

// legalFooter matches the confidentiality boilerplate corporate mail systems
// append. Anchored at a line start, and only honoured near the end of a
// message (see Clean), so a sentence that merely mentions confidentiality
// mid-conversation is not treated as a footer.
var legalFooter = regexp.MustCompile(`(?im)^\s*(` +
	`this (e-?mail|message)( and any attachments)? (is|are|may be) (confidential|intended|privileged)` +
	`|confidentiality notice` +
	`|the information (contained )?in this (e-?mail|message)` +
	`|if you (are not the intended recipient|have received this (e-?mail|message) in error)` +
	`|disclaimer\s*:` +
	`|please consider the environment before printing` +
	`)`)

// legalFooterMaxTail is how much text may follow a footer match for it still
// to count as a footer. Beyond this it is mid-body prose, and cutting there
// would throw away the conversation.
const legalFooterMaxTail = 800

// Clean normalises a message body for extraction.
//
// It deliberately does NOT strip signatures. A signature is where the phone
// number, the company and the job title live, and on a great many leads it is
// the only place the contact details appear at all — the message says "can you
// help with this?" and the signature says who is asking. Removing it to tidy
// the text up would destroy the extraction it was meant to improve.
//
// Only three things go: quote markers (de-quoted, not deleted, because the
// quoted text is frequently the enquiry itself), trailing legal boilerplate,
// and excess whitespace.
func Clean(raw string) string {
	s := normalise(raw)
	s = dequote(s)
	s = stripLegalFooter(s)
	s = tidy(s)
	return s
}

// stripLegalFooter cuts a confidentiality notice and everything after it, but
// only when it really is a footer rather than a passing mention.
func stripLegalFooter(s string) string {
	loc := legalFooter.FindStringIndex(s)
	if loc == nil {
		return s
	}
	if runeLen(s[loc[0]:]) > legalFooterMaxTail {
		// Too much follows for this to be a trailing footer.
		return s
	}
	return strings.TrimSpace(s[:loc[0]])
}
