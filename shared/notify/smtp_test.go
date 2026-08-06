package notify

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// smtpSink is a throwaway in-process SMTP server that captures one message's envelope recipient
// and DATA body, speaking just enough of the protocol for net/smtp's SendMail. Hermetic -- proves
// the real net/smtp path + the envelope recipient without a live relay ([[verification-assertions-must-fail]]:
// assert the address that actually went on the wire, not just "no error").
type smtpSink struct {
	ln   net.Listener
	rcpt string
	data string
	done chan struct{}
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln, done: make(chan struct{})}
	go s.serve()
	return s
}

func (s *smtpSink) serve() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	reply := func(line string) { fmt.Fprintf(conn, "%s\r\n", line) }
	reply("220 sink ready")

	inData := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				s.data = body.String()
				reply("250 queued")
				continue
			}
			body.WriteString(line)
			continue
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 sink") // single-line 250 = no extensions (no STARTTLS/AUTH to negotiate)
		case strings.HasPrefix(cmd, "RCPT TO"):
			if _, addr, ok := strings.Cut(line, ":"); ok {
				s.rcpt = strings.Trim(strings.TrimSpace(addr), "<>")
			}
			reply("250 OK")
		case cmd == "DATA":
			reply("354 go ahead")
			inData = true
		case cmd == "QUIT":
			reply("221 bye")
			return
		default: // MAIL FROM, RSET, NOOP, …
			reply("250 OK")
		}
	}
}

// A configured SMTP notifier actually delivers to the recipient, with Subject = the alert title.
func TestSMTPDeliversToRecipient(t *testing.T) {
	sink := newSMTPSink(t)
	defer sink.ln.Close()

	n := SMTP(SMTPConfig{Addr: sink.ln.Addr().String(), From: "briard@cloud"}, "owner@home.example")
	if err := n.Notify(context.Background(), Alert{
		Level: Warning, Title: "Briard fleet: DEGRADED", Body: "one node down",
	}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	<-sink.done

	if sink.rcpt != "owner@home.example" {
		t.Errorf("envelope recipient = %q, want owner@home.example", sink.rcpt)
	}
	if !strings.Contains(sink.data, "Subject: Briard fleet: DEGRADED\r\n") {
		t.Errorf("message missing Subject header; got:\n%s", sink.data)
	}
	if !strings.Contains(sink.data, "one node down") {
		t.Errorf("message missing body; got:\n%s", sink.data)
	}
}

// Off by default: an empty relay Addr (or empty recipient) yields Nop -- dev/soak send no mail.
func TestSMTPOffWhenUnconfigured(t *testing.T) {
	if _, ok := SMTP(SMTPConfig{}, "x@y").(nopNotifier); !ok {
		t.Error("empty Addr must yield Nop (email off by default)")
	}
	if _, ok := SMTP(SMTPConfig{Addr: "relay:25"}, "").(nopNotifier); !ok {
		t.Error("empty recipient must yield Nop")
	}
}

// The rendered message is a well-formed RFC 5322 mail: From/To/Subject headers, blank line, body.
func TestSMTPMessageFormat(t *testing.T) {
	s := smtpNotifier{cfg: SMTPConfig{From: "a@b"}, to: "c@d"}
	msg := string(s.message(Alert{Title: "T", Body: "B"}, time.Unix(0, 0).UTC()))
	for _, want := range []string{"From: a@b\r\n", "To: c@d\r\n", "Subject: T\r\n", "\r\nB\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got:\n%s", want, msg)
		}
	}
}
