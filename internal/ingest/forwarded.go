package ingest

import (
	"strings"
)

// Provenance records how the lead's identity was chosen.
//
// It exists because "the lead's email address is wrong" is otherwise an
// unfalsifiable complaint: this says which rule fired, who else was on the
// table, and why the winner won. Extracting the wrong party is a silent
// correctness failure — the row looks perfectly normal — so it has to be
// debuggable after the fact.
type Provenance struct {
	// Method is the rule that decided: header:auto-forward, header:resent,
	// body:gmail, body:apple, body:outlook, or direct.
	Method string `json:"method"`
	// Confidence is high when a header settled it or there was one obvious
	// candidate; lower when the choice involved a guess.
	Confidence string `json:"confidence"`
	// Chose names the selection rule: only, earliest, earliest-not-self,
	// sole-header, newest-fallback.
	Chose string `json:"chose,omitempty"`
	// Candidates are every identity considered, in document order.
	Candidates []string `json:"candidates,omitempty"`
	// ThreadCount is how many messages were found in the body.
	ThreadCount int `json:"thread_count,omitempty"`
	// Notes explain why the winner won and the others lost.
	Notes []string `json:"notes,omitempty"`
}

const (
	confidenceHigh   = "high"
	confidenceMedium = "medium"
	confidenceLow    = "low"
)

// Origin is who the lead is and what the model should read.
type Origin struct {
	Sender  Identity
	Subject string
	Text    string
	// Forwarded reports that the material is a forwarded message or thread
	// rather than something sent to us directly. The extraction prompt is told
	// this, so it knows the lead is the original correspondent.
	Forwarded  bool
	Provenance Provenance
}

// ResolveOrigin decides whose lead this is and what text the model should see.
//
// The header rule is the counter-intuitive one, and getting it backwards is
// the most damaging bug available here. Gmail's AUTOMATIC forwarding (the
// filter/settings kind) PRESERVES the original From: header and adds
// X-Forwarded-For / X-Forwarded-To — and those headers contain the USER's own
// addresses, not the lead's. So their presence means "trust From:", it does
// not mean "read the identity out of the header". Reading the header instead
// would attribute every auto-forwarded lead to the user's own mailbox, and the
// resulting rows look entirely plausible.
//
// Only a MANUAL forward — the user pressing Forward — rewrites From: to the
// user and wraps the original in a body marker block. That is the case body
// parsing exists for.
func ResolveOrigin(m Message, opt Options) Origin {
	own := lowerSet(opt.OwnAddresses)

	// 1. Auto-forward. The original sender is intact in From:.
	if m.HasHeader("x-forwarded-for") || m.HasHeader("x-forwarded-to") {
		return Origin{
			Sender:    m.From,
			Subject:   m.Subject,
			Text:      Clean(m.Text),
			Forwarded: true,
			Provenance: Provenance{
				Method: "header:auto-forward", Confidence: confidenceHigh, Chose: "sole-header",
				Candidates: []string{m.SenderAddress()},
				Notes: []string{
					"X-Forwarded-* present, so the message was auto-forwarded and From: is the original sender",
					"body markers deliberately not parsed: a header-confirmed identity must not be overridden by a guess",
				},
			},
		}
	}

	// 2. Resent-*: RFC 5322 redirection, From: is likewise still the original.
	if m.HasHeader("resent-from") {
		return Origin{
			Sender:    m.From,
			Subject:   m.Subject,
			Text:      Clean(m.Text),
			Forwarded: true,
			Provenance: Provenance{
				Method: "header:resent", Confidence: confidenceHigh, Chose: "sole-header",
				Candidates: []string{m.SenderAddress()},
				Notes:      []string{"Resent-From present; From: remains the original sender"},
			},
		}
	}

	// 3. Manual forward: either the sender is the user themselves, or the body
	// carries a client's forward marker.
	blocks, method := parseForwardBlocks(m.Text)
	if len(blocks) > 0 {
		return fromBlocks(m, blocks, method, own)
	}

	// 4. Direct mail.
	prov := Provenance{Method: "direct", Confidence: confidenceHigh, Chose: "only",
		Candidates: []string{m.SenderAddress()}}
	if own[m.SenderAddress()] {
		// The user mailed their own capture address with no forward markers we
		// recognise — most likely a localised client (see markers.go). Flagged
		// low so the review pane warns rather than quietly making the user
		// their own lead.
		prov.Confidence = confidenceLow
		prov.Notes = append(prov.Notes,
			"sender is the account owner but no forward markers were recognised; the real lead may be inside the body")
	}
	return Origin{Sender: m.From, Subject: m.Subject, Text: Clean(m.Text), Provenance: prov}
}

// fromBlocks picks the lead out of a parsed forwarded thread.
func fromBlocks(m Message, blocks []forwardBlock, method string, own map[string]bool) Origin {
	prov := Provenance{Method: method, ThreadCount: len(blocks)}
	for _, b := range blocks {
		prov.Candidates = append(prov.Candidates, b.From.Address)
	}

	if len(blocks) == 1 {
		b := blocks[0]
		prov.Confidence = confidenceHigh
		prov.Chose = "only"
		if b.From.Address == "" {
			prov.Confidence = confidenceLow
			prov.Notes = append(prov.Notes, "the forwarded header block named no sender")
		}
		return origin(m, b, true, prov)
	}

	// A forwarded thread is a conversation. The lead is the person who made
	// the original approach — the EARLIEST message — not whoever replied most
	// recently. Picking the latest is a silent correctness failure: the row
	// names a real person who simply is not the customer.
	order := earliestFirst(blocks)
	prov.Chose = "earliest"
	prov.Confidence = confidenceHigh
	if !datesUsable(blocks) {
		// Forward chains stack newest-first, so the last block in document
		// order is the oldest message.
		prov.Confidence = confidenceMedium
		prov.Notes = append(prov.Notes,
			"dates in the forwarded headers were unparseable; fell back to document order, where the oldest message is last")
	}

	// Skip the user's own messages: a user forwarding a thread they started
	// would otherwise become their own lead.
	chosen := order[0]
	for _, idx := range order {
		if !own[blocks[idx].From.Address] {
			if idx != order[0] {
				prov.Chose = "earliest-not-self"
				prov.Notes = append(prov.Notes,
					"the earliest message was from the account owner, so the next-earliest correspondent was taken as the lead")
			}
			chosen = idx
			break
		}
	}
	if own[blocks[chosen].From.Address] {
		// Every message in the thread is the user's own. Take the newest and
		// say so rather than confidently returning the user as the lead.
		chosen = order[len(order)-1]
		prov.Chose = "newest-fallback"
		prov.Confidence = confidenceLow
		prov.Notes = append(prov.Notes,
			"every message in the thread was from the account owner; no other correspondent was found")
	}

	return origin(m, blocks[chosen], true, prov)
}

// origin builds the result from the chosen block, falling back to the outer
// message where the block is silent.
func origin(m Message, b forwardBlock, forwarded bool, prov Provenance) Origin {
	o := Origin{Sender: b.From, Subject: b.Subject, Text: Clean(b.Body), Forwarded: forwarded, Provenance: prov}
	if o.Sender.Address == "" {
		o.Sender = m.From
	}
	if strings.TrimSpace(o.Subject) == "" {
		// Strip the client's forward prefix so the subject reads as the
		// original enquiry's.
		o.Subject = stripForwardPrefix(m.Subject)
	}
	if strings.TrimSpace(o.Text) == "" {
		o.Text = Clean(m.Text)
	}
	return o
}

// earliestFirst returns block indices ordered oldest-first, by parsed date
// where available and otherwise by reverse document order (forward chains
// stack newest-first).
func earliestFirst(blocks []forwardBlock) []int {
	idx := make([]int, len(blocks))
	for i := range blocks {
		idx[i] = i
	}
	if !datesUsable(blocks) {
		// Reverse document order: the last block is the original message.
		for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
			idx[i], idx[j] = idx[j], idx[i]
		}
		return idx
	}
	// Insertion sort by date; threads are short.
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && blocks[idx[j]].Date.Before(blocks[idx[j-1]].Date); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
	return idx
}

// datesUsable reports whether every block has a parsed date, which is what
// makes date ordering trustworthy. A partial set is worse than none: it mixes
// two orderings.
func datesUsable(blocks []forwardBlock) bool {
	for _, b := range blocks {
		if b.Date.IsZero() {
			return false
		}
	}
	return true
}

var forwardPrefixes = []string{"fwd:", "fw:", "re:", "tr:", "wg:"}

// stripForwardPrefix removes the client's forward/reply prefixes, repeatedly,
// so "Fwd: Re: Re: Catering" reads as "Catering".
func stripForwardPrefix(subject string) string {
	s := strings.TrimSpace(subject)
	for changed := true; changed; {
		changed = false
		lower := strings.ToLower(s)
		for _, p := range forwardPrefixes {
			if strings.HasPrefix(lower, p) {
				s = strings.TrimSpace(s[len(p):])
				changed = true
				break
			}
		}
	}
	return s
}

func lowerSet(vs []string) map[string]bool {
	out := make(map[string]bool, len(vs))
	for _, v := range vs {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out[v] = true
		}
	}
	return out
}
