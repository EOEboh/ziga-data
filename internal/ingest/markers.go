package ingest

import (
	"regexp"
	"strings"
	"time"
)

// forwardBlock is one message inside a forwarded body: the header block a mail
// client wrote when forwarding, plus the text that followed it.
type forwardBlock struct {
	From    Identity
	To      string
	Subject string
	Date    time.Time
	// DateRaw is kept when Date could not be parsed, so thread ordering can
	// fall back to document order knowingly rather than silently.
	DateRaw string
	Body    string
	// Order is the block's position in the message, 0 first.
	Order int
}

// Forward markers, per client. Anchored per line, case-insensitive.
//
// Localised variants (Outlook's "Von:"/"De:", "Ursprüngliche Nachricht") are
// deliberately NOT matched in v1. Half-matching a localised forward is worse
// than not matching it: it would parse a partial header block and attribute
// the lead to whoever the fragment happened to name. Unmatched, the message
// falls through to direct with low confidence, which surfaces a review flag
// the user can correct in one edit.
var (
	gmailMarker   = regexp.MustCompile(`(?im)^\s*-{3,}\s*Forwarded message\s*-{3,}\s*$`)
	appleMarker   = regexp.MustCompile(`(?im)^\s*Begin forwarded message:\s*$`)
	outlookMarker = regexp.MustCompile(`(?im)^\s*-{2,}\s*Original Message\s*-{2,}\s*$`)

	// A bare header block with no marker line, which Outlook and several
	// webmail clients produce.
	bareHeaderBlock = regexp.MustCompile(`(?im)^\s*From:\s*.+\r?\n\s*(Sent|Date):\s*.+$`)

	headerLine = regexp.MustCompile(`(?i)^\s*(from|to|cc|sent|date|subject|reply-to)\s*:\s*(.*)$`)

	// An address in either "Name <addr@host>" or bare "addr@host" form.
	addressPattern = regexp.MustCompile(`<\s*([^<>@\s]+@[^<>@\s]+)\s*>|([^\s<>,;"]+@[^\s<>,;"]+)`)

	// Quote markers at the start of a line, as ">" or "> > ".
	quotePrefix = regexp.MustCompile(`(?m)^[ \t]*(>[ \t]?)+`)
)

// markerKind names which client's forward syntax matched, for provenance.
type markerKind struct {
	method string
	re     *regexp.Regexp
}

var markerKinds = []markerKind{
	{"body:gmail", gmailMarker},
	{"body:apple", appleMarker},
	{"body:outlook", outlookMarker},
}

// normalise makes a forwarded body safe to pattern-match.
//
// The NBSP replacement is not cosmetic: Apple Mail writes its forwarded header
// block with U+00A0 after the field names, so "From: Ada <ada@x>" never
// matches a pattern expecting a plain space. Every implementation of this gets
// caught by it once.
func normalise(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, " ", " ") // NBSP
	s = strings.ReplaceAll(s, "​", "")  // zero-width space
	return s
}

// parseForwardBlocks finds the forwarded messages inside a body, in document
// order. It returns nil when the body carries no forward markers at all.
func parseForwardBlocks(text string) (blocks []forwardBlock, method string) {
	text = normalise(text)

	// Collect every marker position across all client syntaxes, since a
	// forwarded thread can mix them (someone forwards an Outlook forward).
	type hit struct {
		start, end int
		method     string
	}
	var hits []hit
	for _, k := range markerKinds {
		for _, loc := range k.re.FindAllStringIndex(text, -1) {
			hits = append(hits, hit{loc[0], loc[1], k.method})
		}
	}

	if len(hits) == 0 {
		// No marker line, but possibly a bare header block.
		if loc := bareHeaderBlock.FindStringIndex(text); loc != nil {
			hits = append(hits, hit{loc[0], loc[0], "body:outlook"})
		}
	}
	if len(hits) == 0 {
		return nil, ""
	}

	// Sort by position — a simple insertion sort; there are never many.
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].start < hits[j-1].start; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	method = hits[0].method

	for i, h := range hits {
		end := len(text)
		if i+1 < len(hits) {
			end = hits[i+1].start
		}
		segment := text[h.end:end]
		block := parseBlock(segment)
		block.Order = i
		blocks = append(blocks, block)
	}
	return blocks, method
}

// parseBlock reads the header lines at the top of a forwarded segment and
// treats the remainder as the message body.
func parseBlock(segment string) forwardBlock {
	lines := strings.Split(segment, "\n")
	var b forwardBlock
	i := 0

	// Skip blank lines between the marker and the header block.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	for ; i < len(lines); i++ {
		m := headerLine.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		field, value := strings.ToLower(m[1]), strings.TrimSpace(m[2])
		switch field {
		case "from":
			b.From = parseIdentity(value)
		case "to":
			b.To = value
		case "subject":
			b.Subject = value
		case "sent", "date":
			if t, ok := parseLooseDate(value); ok {
				b.Date = t
			} else {
				b.DateRaw = value
			}
		}
	}
	b.Body = strings.TrimSpace(strings.Join(lines[i:], "\n"))
	return b
}

// parseIdentity pulls a name and address out of a From: value.
func parseIdentity(v string) Identity {
	v = strings.TrimSpace(v)
	var id Identity
	if m := addressPattern.FindStringSubmatch(v); m != nil {
		addr := m[1]
		if addr == "" {
			addr = m[2]
		}
		id.Address = strings.ToLower(strings.Trim(addr, "<>.,;"))
	}
	// The name is whatever precedes the address, minus quotes and brackets.
	name := v
	if id.Address != "" {
		if idx := strings.Index(strings.ToLower(v), id.Address); idx > 0 {
			name = v[:idx]
		} else if id.Address == strings.ToLower(strings.TrimSpace(v)) {
			name = ""
		}
	}
	id.Name = strings.TrimSpace(strings.Trim(strings.TrimSpace(name), `"<>,;`))
	return id
}

// dateLayouts covers what mail clients actually write into a forwarded header
// block, which is not RFC 5322 and varies by client and locale.
var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 at 15:04",
	"Mon, 2 Jan 2006 15:04",
	"2 January 2006 at 15:04:05 MST",
	"2 January 2006 at 15:04",
	"January 2, 2006 3:04 PM",
	"Monday, January 2, 2006 3:04 PM",
	"Mon, Jan 2, 2006 at 3:04 PM",
	"2 Jan 2006 15:04:05 -0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseLooseDate tries the layouts mail clients actually emit.
func parseLooseDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	// Strip a leading weekday-with-comma that some layouts double up on, and
	// any trailing timezone name in parentheses.
	v = strings.TrimSuffix(strings.TrimSpace(regexp.MustCompile(`\([^)]*\)\s*$`).ReplaceAllString(v, "")), ",")
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// dequote strips reply quote markers while keeping the text behind them. The
// content of a quoted block is frequently the lead itself, so deleting quoted
// lines outright would delete the enquiry.
func dequote(s string) string {
	return quotePrefix.ReplaceAllString(s, "")
}
