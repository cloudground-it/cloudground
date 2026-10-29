package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/mailgate"
	"github.com/cloudground-it/cloudground/internal/state"
)

// mailbox records what the panel would have sent.
type mailbox struct {
	mu   sync.Mutex
	sent []outgoing
	err  error
}

func (m *mailbox) send(_ context.Context, o outgoing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, o)
	return nil
}

func (m *mailbox) all() []outgoing {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]outgoing{}, m.sent...)
}

func (m *mailbox) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = nil
}

func useMailbox(s *Server) *mailbox {
	m := &mailbox{}
	s.mailSend = m.send
	return m
}

func setServerRelay(t *testing.T, s *Server, r state.MailRelay, alerts string) {
	t.Helper()
	if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.Mail, p.AlertEmails = r, alerts
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// §14.2 step 5: the recipients, the Bcc header gone, a From and a Date
// added when missing, CRLF line ends; the rest as it came.
func TestPrepareMessage(t *testing.T) {
	raw := "To: Ann <ann@example.com>, bob@example.com\nCc: carl@example.com\nBcc: dee@example.com,\n eve@example.com\nSubject: hi\n\nline one\nline two\n"
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	msg, to, err := prepareMessage([]byte(raw), []string{"zed@example.com"}, true, "shop@example.com", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(to, " "); got != "zed@example.com ann@example.com bob@example.com carl@example.com dee@example.com eve@example.com" {
		t.Fatalf("recipients %s", got)
	}
	s := string(msg)
	for _, want := range []string{"To: Ann <ann@example.com>, bob@example.com\r\n", "Subject: hi\r\n", "From: shop@example.com\r\n", "Date: Thu, 22 Oct 2026 09:00:00 +0000\r\n", "\r\n\r\nline one\r\nline two\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in %q", want, s)
		}
	}
	if strings.Contains(s, "dee@") || strings.Contains(s, "eve@") {
		t.Fatalf("the Bcc header was sent: %q", s)
	}
	if _, _, err := prepareMessage([]byte("Subject: x\n\nbody"), nil, true, "a@example.com", nil, now); err != errNoRecipient {
		t.Fatalf("no recipient: %v", err)
	}
	// Without -t only the named recipients count.
	_, to, _ = prepareMessage([]byte(raw), []string{"zed@example.com"}, false, "a@example.com", nil, now)
	if len(to) != 1 {
		t.Fatalf("without -t: %v", to)
	}
}

// gateRequest is a POST /send as the user a uid stands for.
func gateRequest(uid uint32, query, body string) *http.Request {
	r := httptest.NewRequest("POST", "/send?"+query, strings.NewReader(body))
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, peer{uid: uid}))
}

// §14.2: the gate knows the site by its user, refuses a user that is no
// site's, a staging copy, a site with mail off and one with no relay, and
// sends through the site's relay from the site's sender.
func TestMailGate(t *testing.T) {
	s, _, _ := testServer(t)
	box := useMailbox(s)
	site := seedActiveSite(t, s, "shop.example.com", state.SiteWordPress, "8.4", 0)
	stg := seedActiveSite(t, s, "staging.shop.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = site.ID; st.SystemUser = "cg-site-99"; return nil }); err != nil {
		t.Fatal(err)
	}
	users := map[uint32]string{1001: site.SystemUser, 1099: "cg-site-99", 33: "www-data"}
	s.gateUser = func(uid uint32) (string, error) { return users[uid], nil }
	send := func(uid uint32) (int, string) {
		w := httptest.NewRecorder()
		s.handleGateSend(w, gateRequest(uid, "t=1", "To: ann@example.com\nBcc: bob@example.com\nSubject: Order\n\nThanks\n"))
		return w.Code, w.Body.String()
	}
	if code, body := send(33); code != http.StatusForbidden || !strings.Contains(body, "not a site's") {
		t.Fatalf("another user: %d %s", code, body)
	}
	if code, body := send(1099); code != http.StatusForbidden || !strings.Contains(body, "staging") {
		t.Fatalf("staging: %d %s", code, body)
	}
	if code, body := send(1001); code != http.StatusPreconditionFailed {
		t.Fatalf("no relay: %d %s", code, body)
	}
	setServerRelay(t, s, state.MailRelay{Host: "smtp.example.net", From: "relay@example.com"}, "")
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.Mail.From = "shop@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, body := send(1001); code != http.StatusOK || body != "{\"sent\":2}\n" {
		t.Fatalf("send: %d %s", code, body)
	}
	got := box.all()
	if len(got) != 1 || got[0].from != "shop@example.com" || strings.Join(got[0].to, ",") != "ann@example.com,bob@example.com" ||
		got[0].relay.Host != "smtp.example.net" || strings.Contains(string(got[0].msg), "Bcc") {
		t.Fatalf("sent %+v", got)
	}
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error { st.Config.Mail.Off = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, body := send(1001); code != http.StatusForbidden || !strings.Contains(body, "turned off") {
		t.Fatalf("off: %d %s", code, body)
	}
	if w := httptest.NewRecorder(); true {
		r := httptest.NewRequest("POST", "/send?t=1", strings.NewReader("To: a@example.com\n\nx"))
		s.handleGateSend(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("no peer: %d", w.Code)
		}
	}
}

// §14.2: over a real Unix socket the gate reads the connecting process's
// uid from the kernel, and the client's message arrives.
func TestMailGateOverTheSocket(t *testing.T) {
	s, _, _ := testServer(t)
	box := useMailbox(s)
	site := seedActiveSite(t, s, "sock.example.com", state.SiteWordPress, "8.4", 0)
	setServerRelay(t, s, state.MailRelay{Host: "smtp.example.net", From: "relay@example.com"}, "")
	me := uint32(os.Getuid())
	s.gateUser = func(uid uint32) (string, error) {
		if uid == me {
			return site.SystemUser, nil
		}
		return "", fmt.Errorf("uid %d", uid)
	}
	sock := filepath.Join(t.TempDir(), "mail.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.MailRoutes(), ConnContext: MailConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	err = mailgate.Send(context.Background(), sock, mailgate.Request{FromHeaders: true}, []byte("To: ann@example.com\nSubject: hi\n\nhello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := box.all(); len(got) != 1 || got[0].to[0] != "ann@example.com" || got[0].from != "relay@example.com" {
		t.Fatalf("sent %+v", got)
	}
}

// fakeSMTP is a relay that accepts one message without TLS and records
// the conversation.
func fakeSMTP(t *testing.T) (addr string, got func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var log strings.Builder
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		say := func(s string) { fmt.Fprint(c, s+"\r\n") }
		say("220 fake ESMTP")
		data := false
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			mu.Lock()
			log.WriteString(line)
			mu.Unlock()
			switch {
			case data:
				if line == ".\r\n" {
					data = false
					say("250 queued")
				}
			case strings.HasPrefix(line, "EHLO"):
				say("250-fake")
				say("250 8BITMIME")
			case strings.HasPrefix(line, "DATA"):
				data = true
				say("354 go on")
			case strings.HasPrefix(line, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return log.String()
	}
}

// §14.1: the SMTP exchange, and STARTTLS required when asked for.
func TestSMTPSend(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscan(port, &p)
	msg := plainMessage("relay@example.com", []string{"ann@example.com"}, "Prova è", "hello", time.Now())
	err := smtpSend(context.Background(), outgoing{relay: state.MailRelay{Host: host, Port: p, Security: mailNone, From: "relay@example.com"},
		from: "relay@example.com", to: []string{"ann@example.com"}, msg: msg})
	if err != nil {
		t.Fatal(err)
	}
	log := got()
	for _, want := range []string{"MAIL FROM:<relay@example.com>", "RCPT TO:<ann@example.com>", "Subject: =?utf-8?q?Prova_=C3=A8?=", "hello"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	addr, _ = fakeSMTP(t)
	host, port, _ = net.SplitHostPort(addr)
	fmt.Sscan(port, &p)
	err = smtpSend(context.Background(), outgoing{relay: state.MailRelay{Host: host, Port: p, Security: mailStartTLS, From: "relay@example.com"},
		from: "relay@example.com", to: []string{"ann@example.com"}, msg: msg})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a relay without STARTTLS: %v", err)
	}
}

// §12, §14.1: the relay's settings are validated, the password masked.
func TestMailSettings(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	for _, bad := range []map[string]string{
		{"mail_host": "smtp example.net"}, {"mail_port": "70000"}, {"mail_security": "ssl"},
		{"mail_from": "Shop <shop@example.com>"}, {"alert_emails": "a@example.com, nope"},
	} {
		if resp, body := c.do("PUT", "/api/settings", bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", bad, resp.StatusCode, body)
		}
	}
	resp, body := c.do("PUT", "/api/settings", map[string]string{"mail_host": "smtp.example.net", "mail_port": "2525", "mail_security": "tls",
		"mail_username": "u", "mail_password": "secret-pw", "mail_from": "relay@example.com", "alert_emails": "ops@example.com ann@example.com"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"mail_password":"••••••••"`, `"alert_emails":"ops@example.com, ann@example.com"`, `"mail_port":"2525"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("no %s in %s", want, body)
		}
	}
	ps, _ := s.Store.PanelSettings()
	if ps.Mail != (state.MailRelay{Host: "smtp.example.net", Port: 2525, Security: "tls", Username: "u", From: "relay@example.com"}) {
		t.Fatalf("stored %+v", ps.Mail)
	}
}

// §14.3: a site's mail settings; a change of the sender renders the site;
// the site's own relay keeps its password apart; staging is refused.
func TestSiteMailRoutes(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	box := useMailbox(s)
	site := seedActiveSite(t, s, "m.example.com", state.SiteWordPress, "8.4", 0)
	path := fmt.Sprintf("/api/sites/%d/mail", site.ID)
	if resp, body := c.do("PUT", path, map[string]any{"from": "not an address"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad from = %d %s", resp.StatusCode, body)
	}
	agent.forget()
	resp, body := c.do("PUT", path, map[string]any{"from": "shop@m.example.com", "dkim_selector": "s1",
		"relay": map[string]any{"host": "smtp.example.org", "username": "shop", "password": "pw1"}})
	if resp.StatusCode != http.StatusOK || !agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatalf("put = %d %s, rendered %v", resp.StatusCode, body, agent.called(agentrpc.ApplySiteConfig.Name()))
	}
	if !strings.Contains(string(body), `"effective":"site"`) || !strings.Contains(string(body), `"password_set":true`) || strings.Contains(string(body), "pw1") {
		t.Fatalf("view %s", body)
	}
	if pw, _ := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretMailPassword)); pw != "pw1" {
		t.Fatalf("site relay password %q", pw)
	}
	// The selector alone renders nothing.
	agent.forget()
	if resp, _ := c.do("PUT", path, map[string]any{"dkim_selector": "s2"}); resp.StatusCode != http.StatusOK || agent.called(agentrpc.ApplySiteConfig.Name()) {
		t.Fatal("a selector change rendered the site")
	}
	// The test goes through the site's relay from its sender.
	if resp, body := c.do("POST", path+"/test", map[string]string{"to": "ann@example.com"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d %s", resp.StatusCode, body)
	}
	if got := box.all(); len(got) != 1 || got[0].relay.Host != "smtp.example.org" || got[0].password != "pw1" || got[0].from != "shop@m.example.com" {
		t.Fatalf("test sent %+v", got)
	}
	// Dropping the relay clears its password; with no server relay there
	// is none.
	resp, body = c.do("PUT", path, map[string]any{"relay": nil})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"effective":"none"`) {
		t.Fatalf("drop = %d %s", resp.StatusCode, body)
	}
	if pw, _ := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretMailPassword)); pw != "" {
		t.Fatal("the dropped relay's password stayed")
	}
	if resp, _ := c.do("POST", path+"/test", map[string]string{"to": "ann@example.com"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("test with no relay = %d", resp.StatusCode)
	}
	// The checklist reads the sender's domain.
	s.mailTXT = func(_ context.Context, name string) ([]string, error) {
		switch name {
		case "m.example.com":
			return []string{"google-site-verification=x", "v=spf1 include:_spf.example.org ~all"}, nil
		case "s2._domainkey.m.example.com":
			return []string{"v=DKIM1; k=rsa; p=AAAA"}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	resp, body = c.do("GET", path+"/checks", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"spf":{"found":true,"record":"v=spf1 include:_spf.example.org ~all"}`) ||
		!strings.Contains(string(body), `"dmarc":{"found":false}`) || !strings.Contains(string(body), `"dkim":{"selector":"s2","found":true}`) {
		t.Fatalf("checks = %d %s", resp.StatusCode, body)
	}
	stg := seedActiveSite(t, s, "staging.m.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(stg.ID, func(st *state.Site) error { st.StagingOf = site.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp, _ := c.do("PUT", fmt.Sprintf("/api/sites/%d/mail", stg.ID), map[string]any{"from": "a@example.com"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("staging = %d", resp.StatusCode)
	}
}

// §14.4: the alerts go out by email in one message, and are marked
// notified only once it was sent.
func TestAlertEmail(t *testing.T) {
	s, _, _ := testServer(t)
	box := useMailbox(s)
	site := activeSite(t, s, "alert.example.com")
	if err := s.Store.SetCertExpiry(site.ID, time.Now().Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	setServerRelay(t, s, state.MailRelay{Host: "smtp.example.net", From: "panel@example.com"}, "ops@example.com, ann@example.com")
	box.err = fmt.Errorf("relay down")
	ctx := context.Background()
	if err := s.runAlertsNotify(ctx, nil); err != nil {
		t.Fatal(err)
	}
	a, ok := certAlert(t, tableAlerts(t, s))
	if !ok {
		t.Fatal("no certificate alert")
	}
	if got, _ := s.Store.GetAlert(a.ID); got.NotifiedAt != nil {
		t.Fatal("an alert was marked notified though its email failed")
	}
	box.err = nil
	s.notifyAlerts(ctx)
	got := box.all()
	if len(got) != 1 || strings.Join(got[0].to, ",") != "ops@example.com,ann@example.com" || !strings.Contains(string(got[0].msg), "alert.example.com") {
		t.Fatalf("sent %+v", got)
	}
	if got, _ := s.Store.GetAlert(a.ID); got.NotifiedAt == nil {
		t.Fatal("the emailed alert is not marked notified")
	}
}

// §14.2 step 5: through the server-wide relay a site sends only as itself;
// another site's or the panel's address is replaced by the site's sender.
func TestMailGateKeepsTheSiteToItsOwnNames(t *testing.T) {
	s, _, _ := testServer(t)
	box := useMailbox(s)
	site := seedActiveSite(t, s, "shop.example.com", state.SiteWordPress, "8.4", 0)
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.Mail.From = "orders@shop.example.com"
		st.Aliases = []string{"shop.example.org"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.gateUser = func(uint32) (string, error) { return site.SystemUser, nil }
	setServerRelay(t, s, state.MailRelay{Host: "smtp.example.net", From: "alerts@panel.example.net"}, "")
	send := func(head string) string {
		t.Helper()
		box.reset()
		w := httptest.NewRecorder()
		s.handleGateSend(w, gateRequest(1001, "t=1", "To: ann@example.com\n"+head+"Subject: x\n\nbody\n"))
		got := box.all()
		if w.Code != http.StatusOK || len(got) != 1 {
			t.Fatalf("%q: %d %s", head, w.Code, w.Body.String())
		}
		return string(got[0].msg)
	}
	for _, own := range []string{"From: Shop <hello@shop.example.com>\n", "From: news@mail.shop.example.org\n", "From: orders@shop.example.com\n"} {
		if msg := send(own); !strings.Contains(msg, strings.TrimSuffix(own, "\n")+"\r\n") || strings.Contains(msg, "Reply-To") {
			t.Errorf("own name %q became %q", own, msg)
		}
	}
	for _, foreign := range []string{"From: billing@other.example.com\n", "From: alerts@panel.example.net\n", "From: x@notshop.example.com\n", "From: a@shop.example.com, b@other.example.com\n"} {
		msg := send(foreign)
		if !strings.Contains(msg, "From: orders@shop.example.com\r\n") || strings.Count(msg, "From:") != 1 || !strings.Contains(msg, "Reply-To: ") {
			t.Errorf("foreign %q became %q", foreign, msg)
		}
	}
	if msg := send("From: hello@shop.example.com\nSender: alerts@panel.example.net\n"); strings.Contains(msg, "Sender:") {
		t.Errorf("a foreign Sender stayed: %q", msg)
	}
	if msg := send("From: billing@other.example.com\nReply-To: help@shop.example.com\n"); strings.Count(msg, "Reply-To:") != 1 || !strings.Contains(msg, "Reply-To: help@shop.example.com") {
		t.Errorf("the site's own Reply-To was replaced: %q", msg)
	}

	// With a relay of its own, the site sends what that relay takes.
	if _, err := s.Store.MutateSite(site.ID, func(st *state.Site) error {
		st.Config.Mail.Relay = &state.MailRelay{Host: "smtp.shop.example.com", From: "orders@shop.example.com"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if msg := send("From: billing@other.example.com\n"); !strings.Contains(msg, "From: billing@other.example.com\r\n") {
		t.Errorf("own relay: %q", msg)
	}
}
