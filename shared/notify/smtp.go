package notify

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPConfig configures outbound email delivery. Addr is host:port; From is the envelope
// + header sender. Username/Password enable PLAIN auth against the relay; leave them empty for an
// unauthenticated relay (a trusted internal MTA, or the tests' local sink). Stdlib net/smtp only
// (CONTRIBUTING.md: no SDK) -- no mail-service SDK.
type SMTPConfig struct {
	Addr     string
	From     string
	Username string
	Password string
}

// SMTP returns a Notifier that emails each Alert to `to` via the configured relay. An empty Addr
// (or empty recipient) yields Nop -- the off-by-default path, so an unconfigured dev/soak controller
// sends no real mail (the alert still logs locally). Delivery is best-effort like every Notifier:
// a send error is returned for the caller to log, never fatal.
func SMTP(cfg SMTPConfig, to string) Notifier {
	if cfg.Addr == "" || to == "" {
		return Nop()
	}
	return smtpNotifier{cfg: cfg, to: to}
}

type smtpNotifier struct {
	cfg SMTPConfig
	to  string
}

func (s smtpNotifier) Notify(ctx context.Context, a Alert) error {
	var auth smtp.Auth
	if s.cfg.Username != "" {
		host, _, err := net.SplitHostPort(s.cfg.Addr)
		if err != nil {
			return fmt.Errorf("smtp addr %q: %w", s.cfg.Addr, err)
		}
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, host)
	}
	msg := s.message(a, time.Now())
	// Smtp.SendMail is blocking and not context-aware; run it off the loop so the caller's
	// timeout ctx bounds the alert path (a slow MTA must not wedge the per-tenant alert tick).
	// The buffered channel lets a late send drain without leaking the goroutine forever.
	errc := make(chan error, 1)
	go func() { errc <- smtp.SendMail(s.cfg.Addr, auth, s.cfg.From, []string{s.to}, msg) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		if err != nil {
			return fmt.Errorf("smtp send to %s: %w", s.to, err)
		}
		return nil
	}
}

// Message renders the alert as an RFC 5322 message (CRLF line endings): Subject is the alert
// title, the body is the alert body. now stamps the Date header (injected so a test is stable).
func (s smtpNotifier) message(a Alert, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", s.to)
	fmt.Fprintf(&b, "Subject: %s\r\n", a.Title)
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(a.Body)
	b.WriteString("\r\n")
	return []byte(b.String())
}
