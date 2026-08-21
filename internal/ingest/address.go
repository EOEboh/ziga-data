// Package ingest turns raw inbound email into either a lead worth extracting
// or a recorded reason why it was not. It is deliberately free of the store,
// the LLM client and net/http: everything it needs is passed in, which is what
// makes the fixture corpus cheap to run and keeps the filtering logic testable
// without a database.
package ingest

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// LocalPartPrefix marks an address as ours at a glance, in a log line or in a
// user's forwarding rule.
const LocalPartPrefix = "lead-"

// localPartEncoding is unpadded lowercase base32. Base32 rather than base64url
// because the address is read aloud, retyped into a forwarding dialog, and
// lowercased by mail clients on the way through: it must survive all three,
// which rules out mixed case and punctuation.
var localPartEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewLocalPart returns an unguessable inbound local part.
//
// Unguessability is the whole security model of the address: it is a
// capability, and anyone who learns it can spend the owning tenant's
// extraction budget. 80 bits of crypto/rand is far beyond reach of the
// per-address rate limit that fronts it.
func NewLocalPart() (string, error) {
	var b [10]byte // 80 bits
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate inbound local part: %w", err)
	}
	return LocalPartPrefix + strings.ToLower(localPartEncoding.EncodeToString(b[:])), nil
}

// SplitAddress splits an email address into its lowercased local part and
// domain. ok is false when the address is not of the form local@domain.
//
// Address comparison is done on the lowercased form throughout: the local part
// is technically case-sensitive per RFC 5321, but no real mail system honours
// that and our generated addresses are lowercase by construction, so treating
// a differently-cased delivery as a different address would only lose mail.
func SplitAddress(addr string) (local, domain string, ok bool) {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", "", false
	}
	local = strings.ToLower(strings.TrimSpace(addr[:at]))
	domain = strings.ToLower(strings.TrimSpace(addr[at+1:]))
	if local == "" || domain == "" {
		return "", "", false
	}
	return local, domain, true
}
