package ingest

import (
	"regexp"
	"strings"
)

// gmailForwardingSender is the address Google sends forwarding confirmations
// from. Matching on the sender rather than on body text is the whole security
// property here: see DetectVerification.
const gmailForwardingSender = "forwarding-noreply@google.com"

var (
	gmailConfirmSubject = regexp.MustCompile(`(?i)forwarding confirmation`)
	// The code appears in the body as "Confirmation code: 123456789" and in
	// the subject as "(#123456789)".
	confirmCodeBody    = regexp.MustCompile(`(?i)confirmation code[:\s]+(\d{6,12})`)
	confirmCodeSubject = regexp.MustCompile(`\(#(\d{6,12})\)`)
	gmailConfirmURL    = regexp.MustCompile(`https://mail\.google\.com/mail/[^\s"'<>)\]]+`)
)

// VerificationSenders are addresses whose mail is a forwarding handshake
// rather than a lead. Blocking one of these would let a user permanently break
// their own setup with a single click, so the block-sender endpoint refuses
// them.
var VerificationSenders = []string{
	gmailForwardingSender,
	"@google.com",
}

// DetectVerification recognises a provider's forwarding-confirmation
// handshake: the message that carries the code or link a user must act on to
// finish setting up automatic forwarding.
//
// It runs before every content filter because the Gmail confirmation comes
// from forwarding-noreply@google.com, which matches the machine-mail noreply
// rule exactly. Quarantining it would break setup for every Gmail user, and
// silently: the one message they are waiting for would be sitting in a list
// they have no reason to open yet.
//
// Detection requires the sender to match. Inferring "this is a confirmation"
// from body text alone would let anyone mail a fake code that our own UI then
// presents to the user as legitimate — we would be running the phishing page
// ourselves.
func DetectVerification(m Message) (code, url string, ok bool) {
	if m.SenderAddress() != gmailForwardingSender {
		return "", "", false
	}
	if !gmailConfirmSubject.MatchString(m.Subject) {
		return "", "", false
	}

	body := m.Text
	if strings.TrimSpace(body) == "" {
		body = m.HTML
	}

	if match := confirmCodeBody.FindStringSubmatch(body); len(match) > 1 {
		code = match[1]
	} else if match := confirmCodeSubject.FindStringSubmatch(m.Subject); len(match) > 1 {
		code = match[1]
	}

	// Recent confirmations sometimes carry only a link and no code, so both
	// are captured and both are surfaced — requiring a code would silently
	// fail on exactly those messages.
	url = gmailConfirmURL.FindString(body)
	if url == "" {
		url = gmailConfirmURL.FindString(m.HTML)
	}
	url = strings.TrimRight(url, ".,;")

	if code == "" && url == "" {
		return "", "", false
	}
	return code, url, true
}

// IsVerificationSender reports whether blocking this pattern would break the
// forwarding handshake.
func IsVerificationSender(pattern string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	for _, s := range VerificationSenders {
		if pattern == s {
			return true
		}
		// Blocking "@google.com" would also take out the confirmation sender.
		if strings.HasPrefix(s, "@") && strings.HasSuffix(pattern, s) {
			return true
		}
		if strings.HasPrefix(pattern, "@") && strings.HasSuffix(s, pattern) {
			return true
		}
	}
	return false
}
