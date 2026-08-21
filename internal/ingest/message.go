package ingest

import (
	"strings"
	"time"
)

// PayloadVersion is the wire version of Message. The mail worker sends it so a
// future shape change can be rolled out without a synchronised deploy.
const PayloadVersion = 1

// Identity is a name/address pair from a message header.
type Identity struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Attachment is attachment metadata. v1 carries no bytes: attachments are out
// of scope, but their presence is what distinguishes "a lead with a logo
// attached" from "a scanned document and no text", which the filters care
// about.
type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
}

// Worker-reported failures. The worker never drops a message it could not
// handle; it reports why and lets this side record it against the user.
const (
	WorkerEventSizeRejected = "size_rejected"
	WorkerEventParseFailed  = "parse_failed"
)

// Message is one parsed inbound email, exactly as the mail worker delivers it.
//
// This type is the contract between worker/email-ingest and this package. The
// fixtures in testdata/corpus/*.json are literal instances of it, and the
// worker's own tests assert it produces them, so a change to the worker's
// output shape fails there rather than silently changing what gets filtered
// here.
type Message struct {
	Version int `json:"v"`

	// To is the envelope recipient, not the To: header. A capture address that
	// was BCC'd has a To: header naming someone else entirely, so resolving
	// the tenant from the header would fail — or worse, resolve to the wrong
	// tenant.
	To           string `json:"to"`
	EnvelopeFrom string `json:"envelope_from"`

	MessageID string    `json:"message_id"`
	From      Identity  `json:"from"`
	ReplyTo   Identity  `json:"reply_to"`
	Subject   string    `json:"subject"`
	Date      time.Time `json:"date"`
	// ReceivedAt is when the worker handled the message.
	ReceivedAt time.Time `json:"received_at"`

	// Headers holds only the whitelisted headers the filters read, keyed
	// lowercase. It is a whitelist rather than everything so the payload has a
	// bounded size and an unbounded header cannot be used to inflate it.
	Headers map[string][]string `json:"headers"`

	Text        string       `json:"text"`
	HTML        string       `json:"html"`
	Attachments []Attachment `json:"attachments"`

	RawSize int `json:"raw_size"`
	// Truncated reports that the worker cut the body short.
	Truncated bool `json:"truncated"`
	// WorkerEvent is set when the worker could not deliver a usable message.
	WorkerEvent string `json:"worker_event"`
}

// Header returns the first value of a header, case-insensitively.
func (m *Message) Header(name string) string {
	vs := m.Headers[strings.ToLower(name)]
	if len(vs) == 0 {
		return ""
	}
	return strings.TrimSpace(vs[0])
}

// HasHeader reports whether a header is present with a non-empty value.
func (m *Message) HasHeader(name string) bool {
	return m.Header(name) != ""
}

// SenderAddress is the From: address, lowercased and trimmed.
func (m *Message) SenderAddress() string {
	return strings.ToLower(strings.TrimSpace(m.From.Address))
}

// SenderLocalPart is the local part of the From: address, lowercased.
func (m *Message) SenderLocalPart() string {
	local, _, ok := SplitAddress(m.From.Address)
	if !ok {
		return ""
	}
	return local
}
