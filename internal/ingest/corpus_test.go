package ingest

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the .want.json golden files from current behaviour")

const corpusDir = "testdata/corpus"

// want is the expected outcome for one fixture, as stored in NAME.want.json.
// It mirrors Outcome but keeps Text out of the golden file — the exact cleaned
// body is long and churns; what must be pinned is the decision and why.
type want struct {
	Decision string `json:"decision"` // accept | quarantine | verification
	Reason   string `json:"reason,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// TextPrefix pins enough of the cleaned body to catch a converter
	// regression without making the golden file a copy of the message.
	TextPrefix string `json:"text_prefix,omitempty"`
	TextRunes  int    `json:"text_runes,omitempty"`
	Code       string `json:"code,omitempty"`
	URL        string `json:"url,omitempty"`

	// Origin is set on accepted messages: who the lead was decided to be and
	// how. This is the half of the corpus that guards against extracting the
	// wrong party, which is a silent failure — the row looks fine.
	Origin *originWant `json:"origin,omitempty"`
}

type originWant struct {
	Sender      string `json:"sender"`
	Subject     string `json:"subject"`
	Forwarded   bool   `json:"forwarded"`
	Method      string `json:"method"`
	Confidence  string `json:"confidence"`
	Chose       string `json:"chose,omitempty"`
	ThreadCount int    `json:"thread_count,omitempty"`
	// TextPrefix pins the start of the cleaned body the model would see.
	TextPrefix string `json:"text_prefix,omitempty"`
	// SignatureKept asserts a detail that only appears in a signature block
	// survived cleaning — signatures are frequently the only place a lead's
	// phone number appears.
	SignatureKept bool `json:"signature_kept,omitempty"`
}

func decisionOf(o Outcome) string {
	switch {
	case o.Accept:
		return "accept"
	case o.Verification:
		return "verification"
	default:
		return "quarantine"
	}
}

func toWant(o Outcome) want {
	w := want{
		Decision: decisionOf(o),
		Reason:   string(o.Reason),
		Detail:   o.Detail,
		Code:     o.Code,
		URL:      o.URL,
	}
	if o.Text != "" {
		w.TextRunes = runeLen(o.Text)
		w.TextPrefix = prefixRunes(o.Text, 80)
	}
	return w
}

// phoneInSignature is a detail that appears only in a signature block in the
// fixtures that carry one. Clean must not remove it.
var phoneInSignature = regexp.MustCompile(`\+?\d[\d\s()-]{8,}`)

func toOriginWant(og Origin) *originWant {
	return &originWant{
		Sender:        strings.ToLower(og.Sender.Address),
		Subject:       og.Subject,
		Forwarded:     og.Forwarded,
		Method:        og.Provenance.Method,
		Confidence:    og.Provenance.Confidence,
		Chose:         og.Provenance.Chose,
		ThreadCount:   og.Provenance.ThreadCount,
		TextPrefix:    prefixRunes(og.Text, 70),
		SignatureKept: phoneInSignature.MatchString(og.Text),
	}
}

func prefixRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// corpusOptions are the per-user inputs the fixtures assume: one blocked
// sender, and the account owner's own addresses.
func corpusOptions(name string) Options {
	opt := Options{
		OwnAddresses: []string{
			"owner@gmail.com",
			"sam@owner-agency.com",
			"lead-k3m9x7qp2rt4v8wz@in.zigadata.com",
		},
	}
	if strings.HasPrefix(name, "blocked-sender") {
		opt.BlockedSenders = []string{"sales@spammy.example.com"}
	}
	return opt
}

// TestCorpus runs every fixture through Screen and compares the decision
// against its golden file.
//
// Adding a case is dropping three files into testdata/corpus — see the README
// there for the .eml/.json/.want.json contract. Each fixture is a subtest, so
// a regression names the exact property that broke.
func TestCorpus(t *testing.T) {
	names := corpusNames(t)
	if len(names) == 0 {
		t.Fatal("no fixtures found — the corpus is the whole test for this package")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			m := loadMessage(t, name)
			opt := corpusOptions(name)
			outcome := Screen(m, opt)
			got := toWant(outcome)
			// Origin resolution only runs on messages that survive screening —
			// that is the whole ordering guarantee.
			if outcome.Accept {
				resolved := m
				resolved.Text = outcome.Text
				got.Origin = toOriginWant(ResolveOrigin(resolved, opt))
			}

			goldenPath := filepath.Join(corpusDir, name+".want.json")
			if *update {
				writeGolden(t, goldenPath, got)
				return
			}

			raw, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden file (run with -update to create it): %v", err)
			}
			var expected want
			if err := json.Unmarshal(raw, &expected); err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			if !reflect.DeepEqual(got, expected) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(expected, "", "  ")
				t.Errorf("outcome changed.\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

func corpusNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && !strings.HasSuffix(n, ".want") {
			names = append(names, n)
		}
	}
	return names
}

func loadMessage(t *testing.T, name string) Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s.json: %v", name, err)
	}
	return m
}

func writeGolden(t *testing.T, path string, w want) {
	t.Helper()
	out, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorpusHasAnEmlForEveryCase keeps the cross-language contract intact: the
// worker's tests read these .eml files and assert they parse to the committed
// .json. A case with no .eml is one the worker side cannot verify.
func TestCorpusHasAnEmlForEveryCase(t *testing.T) {
	for _, name := range corpusNames(t) {
		if _, err := os.Stat(filepath.Join(corpusDir, name+".eml")); err != nil {
			t.Errorf("%s has no .eml; the worker's parser tests cannot cover it", name)
		}
	}
}

// TestCorpusCoversEveryReason fails when a Reason has no fixture. An
// unexercised filter is one nobody will notice breaking.
func TestCorpusCoversEveryReason(t *testing.T) {
	// rate_limited is produced by the caller (it needs the store), not Screen,
	// and is covered in the httpapi tests instead.
	required := []Reason{
		ReasonBlockedSender, ReasonMachineMail, ReasonCalendar,
		ReasonNoText, ReasonTooShort, ReasonSizeRejected,
	}
	seen := map[Reason]bool{}
	for _, name := range corpusNames(t) {
		o := Screen(loadMessage(t, name), corpusOptions(name))
		if o.Quarantine {
			seen[o.Reason] = true
		}
	}
	for _, r := range required {
		if !seen[r] {
			t.Errorf("no fixture produces reason %q — that filter is untested", r)
		}
	}
}
