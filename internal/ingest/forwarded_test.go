package ingest

import (
	"strings"
	"testing"
)

var ownerOpts = Options{OwnAddresses: []string{"owner@gmail.com", "sam@owner-agency.com"}}

// TestAutoForwardTrustsTheFromHeader is the single most consequential
// assertion in this package.
//
// Gmail's automatic forwarding PRESERVES the original From: and adds
// X-Forwarded-For / X-Forwarded-To — headers that contain the USER's own
// addresses. The intuitive reading ("a forwarding header means the sender is
// the forwarder, look inside") is exactly backwards, and acting on it would
// attribute every auto-forwarded lead to the user's own mailbox while
// producing rows that look completely normal.
func TestAutoForwardTrustsTheFromHeader(t *testing.T) {
	m := Message{
		From:    Identity{Name: "Chiamaka Eze", Address: "chiamaka@eze-events.com"},
		Subject: "Event branding",
		Headers: map[string][]string{
			// Note: both values are the USER's addresses, not the lead's.
			"x-forwarded-for": {"owner@gmail.com lead-abc@in.example.com"},
			"x-forwarded-to":  {"lead-abc@in.example.com"},
		},
		Text: "I'm organising a conference and need full event branding. What would that cost?",
	}

	got := ResolveOrigin(m, ownerOpts)
	if got.Sender.Address != "chiamaka@eze-events.com" {
		t.Fatalf("lead = %q, want the original sender. Reading the identity out of X-Forwarded-* instead of trusting From: attributes every auto-forwarded lead to the user themselves.",
			got.Sender.Address)
	}
	if got.Sender.Address == "owner@gmail.com" {
		t.Fatal("the lead was resolved to the account owner — the auto-forward rule is inverted")
	}
	if !got.Forwarded {
		t.Error("an auto-forwarded message must be marked forwarded so the prompt knows")
	}
	if got.Provenance.Method != "header:auto-forward" || got.Provenance.Confidence != confidenceHigh {
		t.Errorf("provenance = %+v, want a high-confidence header decision", got.Provenance)
	}
}

// TestAutoForwardIgnoresBodyMarkers: an auto-forwarded message can still
// contain quoted forward markers from earlier in its life. A header-confirmed
// identity must not be overridden by a guess parsed out of the body.
func TestAutoForwardIgnoresBodyMarkers(t *testing.T) {
	m := Message{
		From:    Identity{Name: "Chiamaka Eze", Address: "chiamaka@eze-events.com"},
		Subject: "Fwd: Event branding",
		Headers: map[string][]string{"x-forwarded-for": {"owner@gmail.com lead-abc@in.example.com"}},
		Text: "Forwarding what my colleague sent me:\n\n" +
			"---------- Forwarded message ---------\n" +
			"From: Someone Else <someone@elsewhere.example.com>\n" +
			"Date: Mon, 17 Aug 2026 at 09:05\n" +
			"Subject: Event branding\n\n" +
			"Original text here.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Sender.Address != "chiamaka@eze-events.com" {
		t.Errorf("lead = %q, want the header-confirmed sender; body markers must not override a header decision",
			got.Sender.Address)
	}
}

// TestManualForwardReadsTheBody is the other half: a user pressing Forward
// DOES rewrite From: to themselves, and the real lead is inside.
func TestManualForwardReadsTheBody(t *testing.T) {
	m := Message{
		From:    Identity{Name: "Sam Owner", Address: "owner@gmail.com"},
		Subject: "Fwd: Need a logo for my agency",
		Text: "Passing this on.\n\n---------- Forwarded message ---------\n" +
			"From: Ngozi Umeh <ngozi@umeh-legal.com>\n" +
			"Date: Tue, 18 Aug 2026 at 14:02\n" +
			"Subject: Need a logo for my agency\n\n" +
			"I'm starting a legal consultancy and need a logo plus letterhead.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Sender.Address != "ngozi@umeh-legal.com" {
		t.Fatalf("lead = %q, want the forwarded sender, not the user who forwarded it", got.Sender.Address)
	}
	if got.Subject != "Need a logo for my agency" {
		t.Errorf("subject = %q, want the original enquiry's subject without the Fwd: prefix", got.Subject)
	}
	if strings.Contains(got.Text, "Passing this on") {
		t.Error("the forwarder's own preamble should not be part of the lead body")
	}
}

// TestForwardedThreadPicksTheEarliestMessage: a thread is a conversation, and
// the lead is whoever made the original approach. Picking the most recent
// reply names a real person who simply is not the customer — a wrong answer
// that looks entirely plausible in a spreadsheet.
func TestForwardedThreadPicksTheEarliestMessage(t *testing.T) {
	m := Message{
		From:    Identity{Name: "Sam Owner", Address: "owner@gmail.com"},
		Subject: "Fwd: Re: Re: Catering for staff retreat",
		Text: "---------- Forwarded message ---------\n" +
			"From: Bisi Adeleke <bisi@adeleke-catering.com>\n" +
			"Date: Mon, 17 Aug 2026 at 16:40\n" +
			"Subject: Re: Re: Catering\n\n" +
			"Thanks Kunle, I'll send the quote over tomorrow.\n\n" +
			"---------- Forwarded message ---------\n" +
			"From: Kunle Sanni <kunle@zenithhr.com>\n" +
			"Date: Mon, 17 Aug 2026 at 15:10\n" +
			"Subject: Re: Catering\n\n" +
			"That works. Around 60 people.\n\n" +
			"---------- Forwarded message ---------\n" +
			"From: Bisi Adeleke <bisi@adeleke-catering.com>\n" +
			"Date: Mon, 17 Aug 2026 at 09:05\n" +
			"Subject: Catering for staff retreat\n\n" +
			"I'd like to book catering for our staff retreat on 12 September.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Provenance.ThreadCount != 3 {
		t.Fatalf("thread count = %d, want 3", got.Provenance.ThreadCount)
	}
	if got.Provenance.Chose != "earliest" {
		t.Errorf("chose = %q, want earliest", got.Provenance.Chose)
	}
	// The body must be the ORIGINAL enquiry, not the latest reply — that is
	// what carries the actual need.
	if !strings.Contains(got.Text, "book catering for our staff retreat") {
		t.Errorf("body = %q, want the original enquiry", got.Text)
	}
	if strings.Contains(got.Text, "send the quote over tomorrow") {
		t.Error("the most recent reply was chosen instead of the original enquiry")
	}
}

// TestThreadSkipsTheUsersOwnMessages: a user forwarding a thread they started
// would otherwise become their own lead — an entire class of garbage rows.
func TestThreadSkipsTheUsersOwnMessages(t *testing.T) {
	m := Message{
		From:    Identity{Name: "Sam Owner", Address: "owner@gmail.com"},
		Subject: "Fwd: Re: Following up on your brochure",
		Text: "---------- Forwarded message ---------\n" +
			"From: Halima Yusuf <halima@yusuf-textiles.com>\n" +
			"Date: Tue, 18 Aug 2026 at 11:30\n" +
			"Subject: Re: Following up\n\n" +
			"Yes please — we'd want 500 brochures printed.\n\n" +
			"---------- Forwarded message ---------\n" +
			"From: Sam Owner <owner@gmail.com>\n" +
			"Date: Mon, 17 Aug 2026 at 08:00\n" +
			"Subject: Following up\n\n" +
			"Hi Halima, just checking whether you still need print work done.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Sender.Address == "owner@gmail.com" {
		t.Fatal("the account owner became their own lead")
	}
	if got.Sender.Address != "halima@yusuf-textiles.com" {
		t.Fatalf("lead = %q, want the other correspondent", got.Sender.Address)
	}
	if got.Provenance.Chose != "earliest-not-self" {
		t.Errorf("chose = %q, want earliest-not-self so the decision is explicable", got.Provenance.Chose)
	}
}

// TestThreadOfOnlyOwnMessagesDegradesLoudly: when every candidate is the user,
// there is no good answer. Say so with low confidence rather than confidently
// returning the user as the lead.
func TestThreadOfOnlyOwnMessagesDegradesLoudly(t *testing.T) {
	m := Message{
		From:    Identity{Address: "owner@gmail.com"},
		Subject: "Fwd: notes",
		Text: "---------- Forwarded message ---------\n" +
			"From: Sam Owner <owner@gmail.com>\n" +
			"Date: Tue, 18 Aug 2026 at 11:30\n\n" +
			"Second note to self about the pitch deck.\n\n" +
			"---------- Forwarded message ---------\n" +
			"From: Sam Owner <sam@owner-agency.com>\n" +
			"Date: Mon, 17 Aug 2026 at 08:00\n\n" +
			"First note to self about the pitch deck.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Provenance.Confidence != confidenceLow {
		t.Errorf("confidence = %q, want low when no real correspondent was found", got.Provenance.Confidence)
	}
	if len(got.Provenance.Notes) == 0 {
		t.Error("a low-confidence decision must explain itself")
	}
}

// TestAppleMailNBSPHeaders: Apple Mail writes its forwarded header block with
// U+00A0 after the field names. Without normalising it the From: line never
// matches and the lead silently becomes the forwarder.
func TestAppleMailNBSPHeaders(t *testing.T) {
	m := Message{
		From:    Identity{Address: "owner@gmail.com"},
		Subject: "Fwd: Interior design consult",
		Text: "Begin forwarded message:\n\n" +
			"From: Ifeoma Nwosu <ifeoma@nwosu-interiors.com>\n" +
			"Subject: Interior design consult\n" +
			"Date: 17 August 2026 at 10:22:41 WAT\n\n" +
			"We've just taken a new office and need it fitted out.",
	}
	got := ResolveOrigin(m, ownerOpts)
	if got.Sender.Address != "ifeoma@nwosu-interiors.com" {
		t.Fatalf("lead = %q — the non-breaking spaces in Apple Mail's header block were not handled", got.Sender.Address)
	}
	if got.Provenance.Method != "body:apple" {
		t.Errorf("method = %q, want body:apple", got.Provenance.Method)
	}
}

// TestLocalisedForwardDegradesVisibly: localised Outlook markers are out of
// scope for v1. What matters is that an unrecognised forward is FLAGGED rather
// than quietly attributed to the forwarder.
func TestLocalisedForwardDegradesVisibly(t *testing.T) {
	m := Message{
		From:    Identity{Address: "owner@gmail.com"},
		Subject: "WG: Anfrage",
		Text: "-----Ursprüngliche Nachricht-----\n" +
			"Von: Klaus Weber <klaus@weber-bau.de>\n" +
			"Gesendet: Montag, 17. August 2026 14:14\n" +
			"Betreff: Anfrage\n\n" +
			"Wir brauchen eine neue Website für unser Bauunternehmen.",
	}
	got := ResolveOrigin(m, ownerOpts)
	// It is allowed to get the identity wrong here. It is NOT allowed to be
	// confident about it: low confidence raises a review-pane flag the user
	// corrects in one edit.
	if got.Provenance.Confidence != confidenceLow {
		t.Errorf("confidence = %q, want low for an unrecognised forward from the account owner", got.Provenance.Confidence)
	}
	if len(got.Provenance.Notes) == 0 {
		t.Error("the user must be told why this was uncertain")
	}
}

// TestCleanKeepsSignatures: a signature is where the phone number, company and
// title live, and on many leads it is the ONLY place contact details appear.
// Stripping it to tidy the text up destroys the extraction it was meant to help.
func TestCleanKeepsSignatures(t *testing.T) {
	body := "Good afternoon,\n\nWe would like a booking system for our clinic.\n\n" +
		"--\nDr. Femi Adeyemi\nMedical Director | ClinicPlus\nMobile: +234 809 555 0176\nclinicplus.ng"

	got := Clean(body)
	for _, must := range []string{"+234 809 555 0176", "Dr. Femi Adeyemi", "ClinicPlus", "Medical Director"} {
		if !strings.Contains(got, must) {
			t.Errorf("cleaning removed %q, which may be the only contact detail on the lead", must)
		}
	}
}

func TestCleanStripsQuoteMarkersNotContent(t *testing.T) {
	got := Clean("> Hello,\n>\n> We are opening a second shop and need signage.\n> Grace Ademola\n> 0814 555 0177")
	if strings.Contains(got, ">") {
		t.Errorf("quote markers remain: %q", got)
	}
	// The quoted text is frequently the enquiry itself, so it must survive.
	for _, must := range []string{"opening a second shop", "Grace Ademola", "0814 555 0177"} {
		if !strings.Contains(got, must) {
			t.Errorf("de-quoting removed %q", must)
		}
	}
}

func TestCleanStripsTrailingLegalBoilerplate(t *testing.T) {
	body := "Please send a proposal for the portal redesign.\n\nRegards,\nEmeka Obi\n0703 555 0155\n\n" +
		"This email and any attachments are confidential and intended solely for the addressee. " +
		"If you have received this email in error please notify the sender immediately."
	got := Clean(body)
	if strings.Contains(got, "confidential and intended solely") {
		t.Error("the trailing legal footer should be removed — it is boilerplate that never contains lead detail")
	}
	// But the signature just above it must survive.
	if !strings.Contains(got, "0703 555 0155") || !strings.Contains(got, "Emeka Obi") {
		t.Errorf("footer removal took the signature with it: %q", got)
	}
}

func TestCleanKeepsMidBodyMentionsOfConfidentiality(t *testing.T) {
	// A footer pattern appearing mid-conversation is not a footer. Cutting
	// there would throw away the rest of the enquiry.
	body := "Hello,\n\nThis email is confidential, but I wanted to ask about a project.\n\n" +
		strings.Repeat("We need a full rebrand across signage, print and web, with a launch in March. ", 20) +
		"\n\nPlease call me on 0801 555 0100."
	got := Clean(body)
	if !strings.Contains(got, "0801 555 0100") {
		t.Error("a mid-body mention of confidentiality truncated the message and lost the contact details")
	}
}
