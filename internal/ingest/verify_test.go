package ingest

import "testing"

// TestFakeConfirmationIsNeverSurfacedAsVerification is a security assertion,
// not a filtering one.
//
// The forwarding-confirmation UI shows a code and a clickable link with our
// framing around them, telling the user this is the step that completes their
// setup. If detection could be triggered by body text, anyone could mail a
// lookalike and we would render their code and their link inside our own
// product — we would be hosting the phishing page.
//
// So detection requires the real sender. A lookalike is allowed to become an
// ordinary lead: it lands in the review queue where a human reads it and
// discards it, which is the review-first model working as intended. What must
// never happen is it being presented as a verification handshake.
func TestFakeConfirmationIsNeverSurfacedAsVerification(t *testing.T) {
	lookalikes := []Message{
		{
			// Right shape, wrong sender: a homoglyph domain.
			From:    Identity{Address: "security@gmai1-support.example.com"},
			Subject: "Gmail Forwarding Confirmation (#999999999) - Receive Mail from victim@gmail.com",
			Text:    "Confirmation code: 999999999\nhttps://mail.google.com.attacker.example.com/vf-fake",
		},
		{
			// A display name is not an identity.
			From:    Identity{Name: "forwarding-noreply@google.com", Address: "attacker@evil.example.com"},
			Subject: "Gmail Forwarding Confirmation",
			Text:    "Confirmation code: 123456789\nhttps://mail.google.com/mail/vf-real-looking",
		},
		{
			// A subdomain of a domain we trust is still not that domain.
			From:    Identity{Address: "forwarding-noreply@google.com.evil.example.com"},
			Subject: "Gmail Forwarding Confirmation",
			Text:    "Confirmation code: 111222333\nhttps://mail.google.com/mail/vf-x",
		},
	}
	for _, m := range lookalikes {
		code, url, ok := DetectVerification(m)
		if ok {
			t.Errorf("a message from %q was accepted as a forwarding confirmation (code=%q url=%q); our UI would present an attacker's link as the setup step",
				m.From.Address, code, url)
		}
	}
}

func TestRealConfirmationIsDetected(t *testing.T) {
	withCode := Message{
		From:    Identity{Address: "forwarding-noreply@google.com"},
		Subject: "Gmail Forwarding Confirmation (#338214849) - Receive Mail from owner@gmail.com",
		Text: "Confirmation code: 338214849\n\nplease click the link below to confirm:\n" +
			"https://mail.google.com/mail/vf-%5BANGjdJ8x2%5D-DhSl3nQ4kZ\n",
	}
	code, url, ok := DetectVerification(withCode)
	if !ok {
		t.Fatal("the real confirmation must be detected, or no Gmail user can finish setup")
	}
	if code != "338214849" {
		t.Errorf("code = %q, want the confirmation code", code)
	}
	if url == "" {
		t.Error("the confirmation link must be captured too")
	}

	// Newer confirmations sometimes carry only a link. Requiring a code would
	// silently fail on exactly those.
	linkOnly := Message{
		From:    Identity{Address: "forwarding-noreply@google.com"},
		Subject: "Gmail Forwarding Confirmation - Receive Mail from owner@gmail.com",
		Text:    "To allow this, click:\nhttps://mail.google.com/mail/vf-%5BQWxpY2U5OQ%5D-Kp2mR7vT\n",
	}
	code, url, ok = DetectVerification(linkOnly)
	if !ok || url == "" {
		t.Fatalf("a link-only confirmation must still be surfaced: ok=%v url=%q", ok, url)
	}
	if code != "" {
		t.Errorf("code = %q, want empty when the message carries none", code)
	}
}

// TestVerificationOutranksTheMachineFilter is the ordering that decides whether
// this feature works at all for Gmail users.
//
// forwarding-noreply@google.com matches the noreply sender rule exactly. If
// machine-mail filtering ran first, the one message a user must act on to
// finish setup would be quarantined — and silently, because it would sit in a
// list they have no reason to open before their forwarding works.
func TestVerificationOutranksTheMachineFilter(t *testing.T) {
	m := Message{
		To:           "lead-abc@in.example.com",
		EnvelopeFrom: "forwarding-noreply@google.com",
		From:         Identity{Address: "forwarding-noreply@google.com"},
		Subject:      "Gmail Forwarding Confirmation (#338214849) - Receive Mail from owner@gmail.com",
		Text:         "Confirmation code: 338214849\nhttps://mail.google.com/mail/vf-abc\n",
	}

	// Confirm the premise: the machine filter really would catch this.
	if detail, yes := IsMachineMail(m); !yes {
		t.Fatal("premise broken: the confirmation no longer matches a machine-mail rule, so this ordering test proves nothing")
	} else {
		t.Logf("machine filter would have caught it via: %s", detail)
	}

	out := Screen(m, Options{})
	if !out.Verification {
		t.Fatalf("the forwarding confirmation was not surfaced (decision: %+v) — Gmail setup would be impossible", out)
	}
	if out.Code != "338214849" {
		t.Errorf("code = %q", out.Code)
	}
}

// TestBlocklistCannotBreakForwardingSetup: blocking the confirmation sender
// would let a user permanently disable their own setup in one click, with no
// visible cause — they would simply never receive another code.
func TestBlocklistCannotBreakForwardingSetup(t *testing.T) {
	for _, pattern := range []string{
		"forwarding-noreply@google.com",
		"FORWARDING-NOREPLY@GOOGLE.COM",
		"@google.com",
		"  @google.com  ",
	} {
		if !IsVerificationSender(pattern) {
			t.Errorf("blocking %q would break the forwarding handshake but was not recognised", pattern)
		}
	}
	// Ordinary senders must still be blockable, or the feature is useless.
	for _, pattern := range []string{"sales@spammy.example.com", "@spammy.example.com", "noreply@shopify.com"} {
		if IsVerificationSender(pattern) {
			t.Errorf("%q is an ordinary sender and must remain blockable", pattern)
		}
	}
}
