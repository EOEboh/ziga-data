package ingest

import (
	"regexp"
	"strings"
)

// machineLocalParts are sender local parts that essentially never belong to a
// person writing an enquiry.
//
// Some of these are judgement calls — "support@" and "notifications@" are
// occasionally a real human, and a small business owner may well send from
// "info@". That is exactly why a match is a quarantine row with a one-click
// rescue rather than a drop, and why the specific rule that fired is recorded:
// a user repeatedly rescuing the same sender is a support conversation with an
// obvious answer.
var machineLocalParts = map[string]bool{
	"noreply":       true,
	"no-reply":      true,
	"no_reply":      true,
	"donotreply":    true,
	"do-not-reply":  true,
	"mailer-daemon": true,
	"postmaster":    true,
	"bounce":        true,
	"bounces":       true,
	"notification":  true,
	"notifications": true,
	"alerts":        true,
	"newsletter":    true,
	"newsletters":   true,
	"updates":       true,
	"mailer":        true,
	"automated":     true,
}

// bouncePrefix catches VERP-style return paths (bounce-123-abc@…), which vary
// per message and so cannot be listed exhaustively.
var bouncePrefix = regexp.MustCompile(`^bounces?[-+._]`)

// noReplyComponent catches compound no-reply local parts —
// forwarding-noreply@, github-noreply@, noreply-billing@ — which an
// exact-match list misses entirely. These are extremely common and are pure
// cost: every one that slips through is a message billed to the model that was
// never going to be a lead.
//
// It matches only on a whole component (bounded by start, end, or a
// separator), so a person named "Nore Plyms" is not caught by accident.
var noReplyComponent = regexp.MustCompile(`(^|[-._+])(no-?reply|do-?not-?reply)([-._+]|$)`)

// machineSubjects are the subject lines of auto-replies and delivery reports.
var machineSubjects = regexp.MustCompile(`(?i)^\s*(` +
	`out of office` +
	`|automatic reply` +
	`|auto:` +
	`|autoreply` +
	`|undeliverable` +
	`|delivery status notification` +
	`|delivery has failed` +
	`|mail delivery (failed|subsystem)` +
	`|returned mail` +
	`|undelivered mail returned` +
	`|read: ` +
	`|failure notice` +
	`)`)

// bulkPrecedence are the Precedence values that mark mass or automated mail.
var bulkPrecedence = map[string]bool{
	"bulk": true, "list": true, "junk": true, "auto_reply": true, "autoreply": true,
}

// IsMachineMail reports whether a message was sent by a machine rather than a
// person, and which rule said so.
//
// Ordered cheapest first: exact header lookups, then the sender's local part,
// then a regex over the subject. Every message pays this cost, so the order
// matters more than it looks.
func IsMachineMail(m Message) (detail string, yes bool) {
	// RFC 3834: set by auto-responders precisely so recipients can filter.
	if v := strings.ToLower(m.Header("auto-submitted")); v != "" && v != "no" {
		return "Auto-Submitted: " + v, true
	}
	if v := strings.ToLower(m.Header("precedence")); bulkPrecedence[v] {
		return "Precedence: " + v, true
	}
	for _, h := range []string{"x-autoreply", "x-autorespond", "x-auto-response-suppress"} {
		if m.HasHeader(h) {
			return h + " header present", true
		}
	}
	// A List-* header means the sender runs a mailing list. Marketing mail is
	// the single largest source of volume a broad forwarding rule produces, so
	// this is the most valuable check here for cost.
	for _, h := range []string{"list-unsubscribe", "list-id", "list-post"} {
		if m.HasHeader(h) {
			return h + " header present (newsletter or mailing list)", true
		}
	}

	// An empty envelope sender is the null return-path required of bounces and
	// auto-replies, so it cannot loop.
	if strings.TrimSpace(m.EnvelopeFrom) == "" || m.EnvelopeFrom == "<>" {
		return "null return-path (bounce or auto-reply)", true
	}

	local := m.SenderLocalPart()
	if machineLocalParts[local] {
		return "sender local part " + local + "@", true
	}
	if bouncePrefix.MatchString(local) {
		return "bounce-style sender " + local + "@", true
	}
	if noReplyComponent.MatchString(local) {
		return "no-reply sender " + local + "@", true
	}

	if machineSubjects.MatchString(m.Subject) {
		return "subject looks like an auto-reply or delivery report", true
	}

	return "", false
}
