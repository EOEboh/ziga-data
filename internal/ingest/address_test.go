package ingest

import (
	"strings"
	"testing"
)

func TestNewLocalPartIsUnguessableAndMailSafe(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		lp, err := NewLocalPart()
		if err != nil {
			t.Fatal(err)
		}
		if seen[lp] {
			t.Fatalf("generated a duplicate local part %q — the address is a capability and must not repeat", lp)
		}
		seen[lp] = true

		if !strings.HasPrefix(lp, LocalPartPrefix) {
			t.Fatalf("want the %q prefix so an address is recognisable in a log line, got %q", LocalPartPrefix, lp)
		}
		// The address gets read aloud, retyped into a forwarding dialog, and
		// lowercased by mail clients on the way through. Anything outside
		// [a-z0-9-] loses mail on one of those hops.
		for _, r := range lp {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				t.Fatalf("local part %q contains %q, which will not survive a round trip through a mail client", lp, r)
			}
		}
		// 80 bits of base32 is 16 characters, plus the prefix.
		if want := len(LocalPartPrefix) + 16; len(lp) != want {
			t.Fatalf("want length %d (80 bits of entropy), got %d for %q", want, len(lp), lp)
		}
	}
}

func TestSplitAddress(t *testing.T) {
	cases := []struct {
		in           string
		local, domin string
		ok           bool
	}{
		{"lead-abc@in.zigadata.com", "lead-abc", "in.zigadata.com", true},
		// Mail systems vary case freely; our addresses are lowercase by
		// construction, so treating a recased delivery as unknown loses leads.
		{"Lead-ABC@IN.Zigadata.COM", "lead-abc", "in.zigadata.com", true},
		{" lead-abc@in.zigadata.com ", "lead-abc", "in.zigadata.com", true},
		// A local part may itself contain @ in quoted form; the last one wins.
		{`"weird@thing"@in.zigadata.com`, `"weird@thing"`, "in.zigadata.com", true},
		{"no-at-sign", "", "", false},
		{"@nolocal.com", "", "", false},
		{"nodomain@", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		local, domain, ok := SplitAddress(c.in)
		if ok != c.ok || local != c.local || domain != c.domin {
			t.Errorf("SplitAddress(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, local, domain, ok, c.local, c.domin, c.ok)
		}
	}
}
