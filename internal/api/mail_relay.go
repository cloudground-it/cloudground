package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The mail relay (SPEC.md §14.1): the SMTP service the panel hands mail
// to. The panel is not a mail server; it connects to the relay an
// administrator set and sends there, with the password that never leaves
// the panel.

// Timeouts of one hand-over.
const (
	mailDialTimeout = 15 * time.Second
	mailTimeout     = 60 * time.Second
)

// Mail securities.
const (
	mailStartTLS = "starttls"
	mailTLS      = "tls"
	mailNone     = "none"
)

// relayPort is the port of r, its security's when it names none.
func relayPort(r state.MailRelay) int {
	if r.Port != 0 {
		return r.Port
	}
	switch r.Security {
	case mailTLS:
		return 465
	case mailNone:
		return 25
	}
	return 587
}

// outgoing is one message to hand to a relay.
type outgoing struct {
	relay    state.MailRelay
	password string
	// from is the envelope sender.
	from string
	to   []string
	msg  []byte
}

// mailSender sends one message; tests replace it (Server.sendMail).
type mailSender func(ctx context.Context, m outgoing) error

// sendMail is the panel's sender, or the one a test set.
func (s *Server) sendMail(ctx context.Context, m outgoing) error {
	if s.mailSend != nil {
		return s.mailSend(ctx, m)
	}
	return smtpSend(ctx, m)
}

// smtpSend hands m to its relay over SMTP: TLS from the start, or STARTTLS
// required (never a fallback to plain text), or none; AUTH PLAIN when the
// relay has a username.
func smtpSend(ctx context.Context, m outgoing) error {
	r := m.relay
	addr := net.JoinHostPort(r.Host, strconv.Itoa(relayPort(r)))
	ctx, cancel := context.WithTimeout(ctx, mailTimeout)
	defer cancel()
	d := &net.Dialer{Timeout: mailDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("the mail relay %s cannot be reached: %w", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsConf := &tls.Config{ServerName: r.Host, MinVersion: tls.VersionTLS12}
	if r.Security == mailTLS {
		tc := tls.Client(conn, tlsConf)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("the mail relay's TLS: %w", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, r.Host)
	if err != nil {
		return fmt.Errorf("the mail relay: %w", err)
	}
	defer c.Close()
	if err := c.Hello(heloName()); err != nil {
		return fmt.Errorf("the mail relay: %w", err)
	}
	if r.Security == mailStartTLS || r.Security == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail relay does not offer STARTTLS; choose the security it supports")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("the mail relay's STARTTLS: %w", err)
		}
	}
	if r.Username != "" {
		// PlainAuth refuses to send the password over a connection that is
		// not TLS, unless the relay is this machine.
		if err := c.Auth(smtp.PlainAuth("", r.Username, m.password, r.Host)); err != nil {
			return fmt.Errorf("the mail relay refused the sign-in: %w", err)
		}
	}
	if err := c.Mail(m.from); err != nil {
		return fmt.Errorf("the mail relay refused the sender %s: %w", m.from, err)
	}
	for _, to := range m.to {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("the mail relay refused a recipient: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("the mail relay: %w", err)
	}
	if _, err := w.Write(m.msg); err != nil {
		return fmt.Errorf("the mail relay: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail relay refused the message: %w", err)
	}
	return c.Quit()
}

// heloName is the name the panel greets a relay with: this machine's.
func heloName() string {
	if h, err := os.Hostname(); err == nil && h != "" && !strings.ContainsAny(h, " \r\n") {
		return h
	}
	return "localhost"
}

// relayFor is the relay a site's mail goes through and its password: the
// site's own, else the server's (SPEC.md §14.1). scope is "site",
// "server" or "none"; none for a staging copy, a site whose mail is off, or
// when no relay is configured.
func (s *Server) relayFor(site state.Site) (r state.MailRelay, password, scope string, err error) {
	if site.StagingOf != 0 || site.Config.Mail.Off {
		return state.MailRelay{}, "", "none", nil
	}
	if own := site.Config.Mail.Relay; own != nil && own.Configured() {
		pw, err := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretMailPassword))
		return *own, pw, "site", err
	}
	r, password, err = s.serverRelay()
	if err != nil || !r.Configured() {
		return state.MailRelay{}, "", "none", err
	}
	return r, password, "server", nil
}

// serverRelay is the server-wide relay and its password.
func (s *Server) serverRelay() (state.MailRelay, string, error) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return state.MailRelay{}, "", err
	}
	pw, err := s.Store.Secret(state.SecretMailPassword)
	return ps.Mail, pw, err
}

// siteSender is the address a site's mail is sent from: its own, else its
// relay's.
func siteSender(site state.Site, r state.MailRelay) string {
	if site.Config.Mail.From != "" {
		return site.Config.Mail.From
	}
	return r.From
}

// errNoRecipient is a message that names nobody.
var errNoRecipient = errors.New("no recipient")

// maxMailRecipients bounds one message's recipients.
const maxMailRecipients = 50

// siteNamesAddress reports whether addr is on one of the site's names: its
// domain, an alias, or a subdomain of either.
func siteNamesAddress(site state.Site, addr string) bool {
	_, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return false
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	for _, name := range append([]string{site.Domain}, site.Aliases...) {
		if name = strings.ToLower(name); domain == name || strings.HasSuffix(domain, "."+name) {
			return true
		}
	}
	return false
}

// prepareMessage readies a message a site handed over (SPEC.md §14.2 step
// 5): its recipients (those named, and with fromHeaders its To, Cc and
// Bcc), without its Bcc header, with a From and a Date when it has none,
// with CRLF line ends. With own set (the server-wide relay), a From or
// Sender naming an address own refuses is removed, from takes its place,
// and the address given becomes the Reply-To when there is none.
func prepareMessage(raw []byte, named []string, fromHeaders bool, from string, own func(string) bool, now time.Time) (msg []byte, to []string, err error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	head, body, found := bytes.Cut(raw, []byte("\n\n"))
	if !found {
		head, body = bytes.TrimRight(raw, "\n"), nil
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(append(append([]byte{}, head...), "\n\n"...)))
	if err != nil {
		return nil, nil, fmt.Errorf("the message's headers cannot be read: %w", err)
	}
	seen := map[string]bool{}
	add := func(addr string) {
		if a := strings.ToLower(addr); !seen[a] {
			seen[a] = true
			to = append(to, addr)
		}
	}
	for _, n := range named {
		a, err := mail.ParseAddress(n)
		if err != nil {
			return nil, nil, fmt.Errorf("recipient %q is not an address", n)
		}
		add(a.Address)
	}
	if fromHeaders {
		for _, h := range []string{"To", "Cc", "Bcc"} {
			if parsed.Header.Get(h) == "" {
				continue
			}
			list, err := parsed.Header.AddressList(h)
			if err != nil {
				return nil, nil, fmt.Errorf("the %s header cannot be read: %w", h, err)
			}
			for _, a := range list {
				add(a.Address)
			}
		}
	}
	if len(to) == 0 {
		return nil, nil, errNoRecipient
	}
	if len(to) > maxMailRecipients {
		return nil, nil, fmt.Errorf("more than %d recipients", maxMailRecipients)
	}
	// A From or Sender the site may not use goes (SPEC.md §14.2 step 5).
	drop := map[string]bool{"bcc": true}
	var replyTo string
	if own != nil {
		for _, h := range []string{"From", "Sender"} {
			if parsed.Header.Get(h) == "" {
				continue
			}
			list, err := parsed.Header.AddressList(h)
			ok := err == nil && len(list) > 0
			for _, a := range list {
				ok = ok && own(a.Address)
			}
			if !ok {
				drop[strings.ToLower(h)] = true
				if h == "From" && err == nil && len(list) > 0 && parsed.Header.Get("Reply-To") == "" {
					replyTo = list[0].String()
				}
			}
		}
	}
	// The header lines as they came, those dropped (and their
	// continuations) left out, so nothing else of the message is rewritten.
	var out bytes.Buffer
	skipping := false
	for _, line := range bytes.Split(head, []byte("\n")) {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !skipping {
				out.Write(line)
				out.WriteString("\r\n")
			}
			continue
		}
		name, _, _ := bytes.Cut(line, []byte(":"))
		skipping = drop[strings.ToLower(strings.TrimSpace(string(name)))]
		if !skipping {
			out.Write(line)
			out.WriteString("\r\n")
		}
	}
	if parsed.Header.Get("From") == "" || drop["from"] {
		out.WriteString("From: " + from + "\r\n")
	}
	if replyTo != "" {
		out.WriteString("Reply-To: " + replyTo + "\r\n")
	}
	if parsed.Header.Get("Date") == "" {
		out.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	}
	out.WriteString("\r\n")
	out.Write(bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n")))
	return out.Bytes(), to, nil
}

// plainMessage is a short text message the panel itself writes (a test,
// the alerts).
func plainMessage(from string, to []string, subject, body string, now time.Time) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
