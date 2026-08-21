package ingest

import (
	"strings"
	"unicode/utf8"
)

// Reason is why a message did not become a lead. It is what the user sees in
// the quarantine list, so it names a category rather than a rule.
type Reason string

const (
	ReasonBlockedSender Reason = "blocked_sender"
	ReasonMachineMail   Reason = "machine_mail"
	ReasonCalendar      Reason = "calendar_invite"
	ReasonNoText        Reason = "no_text"
	ReasonTooShort      Reason = "too_short"
	ReasonSizeRejected  Reason = "size_rejected"
	ReasonParseFailed   Reason = "parse_failed"
	ReasonRateLimited   Reason = "rate_limited"
)

// Length bounds on the text handed to extraction.
const (
	// MinLeadRunes is the floor below which there is nothing to extract. A
	// two-word body is a "thanks!" or a read receipt, not an enquiry.
	MinLeadRunes = 20
	// MaxLeadRunes caps what reaches the model. Anything longer is a digest,
	// a newsletter or a long thread rather than one lead — and the cost of
	// extracting it scales with its length.
	MaxLeadRunes = 20_000
)

// Outcome is what Screen decided. Exactly one of Accept, Quarantine or
// Verification is true.
type Outcome struct {
	Accept       bool
	Quarantine   bool
	Verification bool

	// Reason is the user-facing category, set on anything but Accept.
	Reason Reason
	// Detail names the exact rule that fired ("Precedence: bulk"), for the
	// support path. It is never the user-facing explanation — Reason is.
	Detail string

	// Text is the cleaned body, set on Accept.
	Text string
	// Truncated reports that Text was cut at MaxLeadRunes.
	Truncated bool

	// Code and URL carry a forwarding-confirmation handshake, set on
	// Verification.
	Code string
	URL  string
}

func quarantine(reason Reason, detail string) Outcome {
	return Outcome{Quarantine: true, Reason: reason, Detail: detail}
}

// Options are the per-user inputs Screen needs. The caller loads them, so this
// package stays free of the store.
type Options struct {
	// BlockedSenders are lowercased addresses ("spam@x.com") and domain
	// suffixes ("@x.com").
	BlockedSenders []string
	// OwnAddresses are the user's own email addresses, used to recognise a
	// message they forwarded themselves.
	OwnAddresses []string
}

// Screen runs the filter pipeline and returns the one outcome.
//
// Order is cheapest-check-first, because this runs on every message and its
// whole purpose is to be cheaper than the model. Every non-Accept outcome
// carries a Reason the caller records against the user: once a message has a
// tenant, it is never dropped, only explained.
//
// Dedup and the per-user cap are deliberately NOT here. They need the store,
// and taking a store interface just to satisfy a diagram would cost every test
// in the corpus a fake. The caller runs them around this.
func Screen(m Message, opt Options) Outcome {
	// 0. The worker could not produce a usable message. There is no body to
	// screen, so this short-circuits — but it is still recorded, because a
	// 30 MB attachment from a real client is a lead the user should know
	// arrived.
	switch m.WorkerEvent {
	case WorkerEventSizeRejected:
		return quarantine(ReasonSizeRejected, "message exceeded the size limit and was not fetched")
	case WorkerEventParseFailed:
		return quarantine(ReasonParseFailed, "message could not be parsed as MIME")
	}

	// 1. The user's own blocklist. A map lookup, and an explicit instruction
	// from the user, so it outranks everything else.
	if pattern, blocked := IsBlockedSender(m.SenderAddress(), opt.BlockedSenders); blocked {
		return quarantine(ReasonBlockedSender, "matched your blocked sender "+pattern)
	}

	// 2. A forwarding-confirmation handshake.
	//
	// This runs BEFORE the machine-mail filter, and the order is load-bearing:
	// the confirmation arrives from forwarding-noreply@google.com, which
	// matches the noreply sender pattern exactly. Filtering it first would
	// quarantine the one message a user must act on to finish setup, breaking
	// the feature for every Gmail user with no visible cause.
	if code, url, ok := DetectVerification(m); ok {
		return Outcome{Verification: true, Reason: "verification", Code: code, URL: url}
	}

	// 3. Machine mail: header lookups, then the sender's local part, then
	// subject patterns.
	if detail, yes := IsMachineMail(m); yes {
		return quarantine(ReasonMachineMail, detail)
	}

	// 4. Structure: a calendar invite or an attachment with no readable text.
	if detail, yes := isCalendarOnly(m); yes {
		return quarantine(ReasonCalendar, detail)
	}

	// 5. Body text, falling back to rendering the HTML part.
	text := strings.TrimSpace(m.Text)
	if runeLen(text) < MinLeadRunes && strings.TrimSpace(m.HTML) != "" {
		if rendered := HTMLToText(m.HTML); runeLen(rendered) > runeLen(text) {
			text = rendered
		}
	}

	if strings.TrimSpace(text) == "" {
		if len(m.Attachments) > 0 {
			return quarantine(ReasonNoText,
				"the message body was empty; attachments are not read in this version")
		}
		return quarantine(ReasonNoText, "the message had no readable body")
	}

	// 6. Length floor. The ceiling is applied by the caller after forwarded
	// parsing, since that step can shrink the text considerably.
	if runeLen(text) < MinLeadRunes {
		return quarantine(ReasonTooShort, "the message body was too short to contain a lead")
	}

	return Outcome{Accept: true, Text: text}
}

// Truncate caps text at MaxLeadRunes on a rune boundary, reporting whether it
// cut. Applied after forwarded parsing, so a long quoted thread that reduces
// to one short message is not needlessly truncated first.
func Truncate(text string) (string, bool) {
	if runeLen(text) <= MaxLeadRunes {
		return text, false
	}
	count := 0
	for i := range text {
		if count == MaxLeadRunes {
			return strings.TrimSpace(text[:i]), true
		}
		count++
	}
	return text, false
}

// IsBlockedSender reports whether an address matches the user's blocklist,
// returning the pattern that matched. Patterns are either a full address or a
// "@domain" suffix.
func IsBlockedSender(address string, patterns []string) (string, bool) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return "", false
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "":
			continue
		case strings.HasPrefix(p, "@"):
			if strings.HasSuffix(address, p) {
				return p, true
			}
		case p == address:
			return p, true
		}
	}
	return "", false
}

// calendarBodyFloor is how much text a message carrying a calendar attachment
// needs before it counts as a real message with an invite attached, rather
// than an invite with boilerplate.
//
// It is well above MinLeadRunes because invite boilerplate is not short: "When:
// Thu Aug 27, 2026 10am / Where: ... / Who: ..." clears any small floor while
// containing nothing extractable.
const calendarBodyFloor = 200

// isCalendarOnly reports a message whose real content is a calendar invite.
// Invites are out of scope: a meeting is not an enquiry, and their generated
// body extracts into a lead whose "need" is a date and whose contact is an
// organiser who never asked for anything.
func isCalendarOnly(m Message) (string, bool) {
	// A top-level text/calendar content type means the message IS the invite,
	// however much boilerplate the text part carries.
	ct := strings.ToLower(m.Header("content-type"))
	if strings.Contains(ct, "text/calendar") || strings.Contains(ct, "application/ics") {
		return "message is a calendar invite (text/calendar)", true
	}

	hasCalendarAttachment := false
	for _, a := range m.Attachments {
		if strings.Contains(strings.ToLower(a.MimeType), "text/calendar") ||
			strings.HasSuffix(strings.ToLower(a.Filename), ".ics") {
			hasCalendarAttachment = true
			break
		}
	}
	if !hasCalendarAttachment {
		return "", false
	}
	// An invite attached to a genuine message ("here's the brief, and a slot
	// for Thursday") is a real lead, so only the boilerplate-only case is
	// filtered.
	if runeLen(strings.TrimSpace(m.Text)) >= calendarBodyFloor {
		return "", false
	}
	return "calendar invite attached with no accompanying message", true
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
