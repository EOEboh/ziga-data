// Package mail sends transactional email (verification and password-reset
// links). It exposes a small Mailer interface with an SMTP implementation for
// production, a dev implementation that logs links when SMTP is unconfigured,
// and a fake for tests.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"sync"
)

// Message is one outbound email. Text is the plaintext fallback; HTML is
// optional.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends a message. Implementations must be safe for concurrent use.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// SMTPMailer sends via an SMTP server (STARTTLS on the standard submission
// port). Credentials come from config; nothing is logged.
type SMTPMailer struct {
	addr string // host:port
	from string
	auth smtp.Auth
}

// NewSMTPMailer builds an SMTP mailer. Auth is omitted when username is empty
// (e.g. a local relay).
func NewSMTPMailer(host, port, username, password, from string) *SMTPMailer {
	var auth smtp.Auth
	if username != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}
	return &SMTPMailer{addr: host + ":" + port, from: from, auth: auth}
}

func (m *SMTPMailer) Send(_ context.Context, msg Message) error {
	return smtp.SendMail(m.addr, m.auth, m.from, []string{msg.To}, buildMIME(m.from, msg))
}

// buildMIME renders a minimal multipart/alternative message (or text/plain
// when there is no HTML part).
func buildMIME(from string, msg Message) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", msg.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	if msg.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		b.WriteString(msg.Text)
		return []byte(b.String())
	}
	const boundary = "ziga-boundary-9f2c"
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", boundary, msg.Text)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s\r\n", boundary, msg.HTML)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// LogMailer is the development fallback used when SMTP is unconfigured: it logs
// each message (including the link in the body) instead of sending, so local
// signup/reset flows are still walkable.
type LogMailer struct{ log *slog.Logger }

func NewLogMailer(log *slog.Logger) *LogMailer { return &LogMailer{log: log} }

func (m *LogMailer) Send(_ context.Context, msg Message) error {
	m.log.Warn("email not sent (SMTP unconfigured); logging instead",
		"to", msg.To, "subject", msg.Subject, "body", msg.Text)
	return nil
}

// FakeMailer records sent messages for assertions in tests.
type FakeMailer struct {
	mu   sync.Mutex
	Sent []Message
}

func (m *FakeMailer) Send(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, msg)
	return nil
}

// Last returns the most recently sent message, or false if none.
func (m *FakeMailer) Last() (Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Sent) == 0 {
		return Message{}, false
	}
	return m.Sent[len(m.Sent)-1], true
}
