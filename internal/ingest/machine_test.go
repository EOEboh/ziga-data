package ingest

import "testing"

func TestMachineMailDetection(t *testing.T) {
	human := Message{
		EnvelopeFrom: "ada@lumen.studio",
		From:         Identity{Name: "Ada Okafor", Address: "ada@lumen.studio"},
		Subject:      "Landing page for our March launch",
		Text:         "We need a landing page built.",
	}
	if detail, yes := IsMachineMail(human); yes {
		t.Fatalf("a plain human enquiry was filtered as machine mail via %q", detail)
	}

	machine := []struct {
		name  string
		mutut func(*Message)
	}{
		{"RFC 3834 auto-responder", func(m *Message) {
			m.Headers = map[string][]string{"auto-submitted": {"auto-replied"}}
		}},
		{"bulk precedence", func(m *Message) {
			m.Headers = map[string][]string{"precedence": {"bulk"}}
		}},
		{"mailing list", func(m *Message) {
			m.Headers = map[string][]string{"list-unsubscribe": {"<https://x.example/u>"}}
		}},
		{"legacy autoreply header", func(m *Message) {
			m.Headers = map[string][]string{"x-autoreply": {"yes"}}
		}},
		{"null return path", func(m *Message) { m.EnvelopeFrom = "" }},
		{"angle-bracket null return path", func(m *Message) { m.EnvelopeFrom = "<>" }},
		{"noreply sender", func(m *Message) { m.From.Address = "noreply@shopify.com" }},
		{"compound noreply sender", func(m *Message) { m.From.Address = "github-noreply@github.com" }},
		{"do-not-reply sender", func(m *Message) { m.From.Address = "do-not-reply@bank.example.com" }},
		{"mailer-daemon", func(m *Message) { m.From.Address = "mailer-daemon@googlemail.com" }},
		{"VERP bounce", func(m *Message) { m.From.Address = "bounce-4471-xyz@mailer.example.com" }},
		{"out of office subject", func(m *Message) { m.Subject = "Out of Office: your enquiry" }},
		{"automatic reply subject", func(m *Message) { m.Subject = "Automatic reply: Landing page" }},
		{"delivery failure subject", func(m *Message) { m.Subject = "Delivery Status Notification (Failure)" }},
		{"read receipt", func(m *Message) { m.Subject = "Read: Landing page for our March launch" }},
	}
	for _, c := range machine {
		t.Run(c.name, func(t *testing.T) {
			m := human
			c.mutut(&m)
			if _, yes := IsMachineMail(m); !yes {
				t.Errorf("not detected as machine mail; every one that slips through is a billed extraction that was never a lead")
			}
		})
	}
}

func TestMachineFilterDoesNotCatchRealPeople(t *testing.T) {
	// These are the false positives that would cost a user actual leads. They
	// are quarantined-not-dropped either way, but a filter that catches
	// ordinary senders makes the quarantine list the primary queue, which
	// defeats the point.
	people := []string{
		"ada@lumen.studio",
		"info@brightpath.ng",       // a small business's shared inbox
		"hello@studio.example.com", // ditto
		"norah.reilly@example.com", // contains "nore" but is a person
		"j.bouncer@example.com",    // contains "bounce" but is a name
		"replies@example.com",      // "replies" is not "no-reply"
	}
	for _, addr := range people {
		m := Message{
			EnvelopeFrom: addr,
			From:         Identity{Address: addr},
			Subject:      "Quick question about your services",
			Text:         "I'd like a quote for some design work.",
		}
		if detail, yes := IsMachineMail(m); yes {
			t.Errorf("%s was filtered as machine mail via %q — that is a lost lead", addr, detail)
		}
	}
}

func TestBlockedSenderMatching(t *testing.T) {
	patterns := []string{"spam@bad.example.com", "@spammy.example.com", "  ", ""}

	if p, ok := IsBlockedSender("spam@bad.example.com", patterns); !ok || p != "spam@bad.example.com" {
		t.Errorf("exact address block failed: %q %v", p, ok)
	}
	// Mail arrives in whatever case the sender used.
	if _, ok := IsBlockedSender("SPAM@BAD.EXAMPLE.COM", patterns); !ok {
		t.Error("address matching must be case-insensitive")
	}
	if p, ok := IsBlockedSender("anyone@spammy.example.com", patterns); !ok || p != "@spammy.example.com" {
		t.Errorf("domain block failed: %q %v", p, ok)
	}
	if _, ok := IsBlockedSender("ada@lumen.studio", patterns); ok {
		t.Error("an unrelated sender must not be blocked")
	}
	// A domain pattern must not match a lookalike that merely ends similarly.
	if _, ok := IsBlockedSender("someone@notspammy.example.com.evil.com", patterns); ok {
		t.Error("suffix matching must not be fooled by a longer domain")
	}
	if _, ok := IsBlockedSender("", patterns); ok {
		t.Error("an empty sender must not match")
	}
}

func TestTruncateAtRuneBoundary(t *testing.T) {
	short := "a short message"
	if got, cut := Truncate(short); cut || got != short {
		t.Errorf("short text must pass through untouched: %q %v", got, cut)
	}

	// Multi-byte runes must not be sliced in half — a broken rune would reach
	// the model as replacement characters.
	long := ""
	for range MaxLeadRunes + 100 {
		long += "é"
	}
	got, cut := Truncate(long)
	if !cut {
		t.Fatal("over-length text must be truncated")
	}
	if runeLen(got) > MaxLeadRunes {
		t.Errorf("truncated to %d runes, want at most %d", runeLen(got), MaxLeadRunes)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}
