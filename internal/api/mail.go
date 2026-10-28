package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's mail and the relay tests (SPEC.md §14.3); the gate is
// mail_gate.go, the relay mail_relay.go.

// mailHostRe is a relay host name; an address is taken by netip.
var mailHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)

// dkimSelectorRe is a DKIM selector (a DNS label or two).
var dkimSelectorRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,62})$`)

// maxAlertEmails bounds the alert recipients.
const maxAlertEmails = 10

// mailCheckTimeout bounds each lookup of the checklist.
const mailCheckTimeout = 3 * time.Second

func checkMailHost(v string) error {
	if v == "" {
		return nil
	}
	if _, err := netip.ParseAddr(v); err == nil {
		return nil
	}
	if len(v) > 253 || !mailHostRe.MatchString(v) {
		return errors.New("must be a host name or an address")
	}
	return nil
}

func parseMailPort(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("must be 1–65535, or empty for the security's port")
	}
	return n, nil
}

func checkMailSecurity(v string) error {
	switch v {
	case "", mailStartTLS, mailTLS, mailNone:
		return nil
	}
	return errors.New("must be starttls, tls or none")
}

// plainAddress reports whether v is one bare email address (no name, no
// angle brackets).
func plainAddress(v string) bool {
	a, err := mail.ParseAddress(v)
	return err == nil && a.Name == "" && a.Address == v && !strings.ContainsAny(v, " \t\r\n\"'<>;,")
}

// parseAlertEmails reads up to maxAlertEmails addresses separated by
// commas or spaces.
func parseAlertEmails(v string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if !plainAddress(f) {
			return nil, fmt.Errorf("%s is not an email address", f)
		}
		out = append(out, f)
	}
	if len(out) > maxAlertEmails {
		return nil, fmt.Errorf("at most %d addresses", maxAlertEmails)
	}
	return out, nil
}

// relayView is a relay as the API shows it: never its password.
type relayView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"`
	Username    string `json:"username"`
	From        string `json:"from"`
	PasswordSet bool   `json:"password_set"`
}

// siteMailView is GET /api/sites/{id}/mail.
type siteMailView struct {
	Off          bool       `json:"off"`
	From         string     `json:"from"`
	DKIMSelector string     `json:"dkim_selector"`
	Relay        *relayView `json:"relay"`
	// Effective is the relay the site's mail goes through: site, server
	// or none; Sender the address it goes from.
	Effective string `json:"effective"`
	Sender    string `json:"sender"`
}

func (s *Server) siteMail(site state.Site) (siteMailView, error) {
	m := site.Config.Mail
	v := siteMailView{Off: m.Off, From: m.From, DKIMSelector: m.DKIMSelector}
	if m.Relay != nil {
		pw, err := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretMailPassword))
		if err != nil {
			return v, err
		}
		v.Relay = &relayView{Host: m.Relay.Host, Port: m.Relay.Port, Security: m.Relay.Security, Username: m.Relay.Username, From: m.Relay.From, PasswordSet: pw != ""}
	}
	r, _, scope, err := s.relayFor(site)
	if err != nil {
		return v, err
	}
	v.Effective = scope
	if scope != "none" {
		v.Sender = siteSender(site, r)
	}
	return v, nil
}

func (s *Server) handleGetSiteMail(w http.ResponseWriter, r *http.Request, c *call) {
	v, err := s.siteMail(c.Site)
	if err != nil {
		internalError(w, "siteMail", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// siteMailChange is the body of PUT /api/sites/{id}/mail. A field left
// out keeps its value; relay null drops the site's own relay.
type siteMailChange struct {
	Off          *bool           `json:"off"`
	From         *string         `json:"from"`
	DKIMSelector *string         `json:"dkim_selector"`
	Relay        json.RawMessage `json:"relay"`
}

// relayChange is a site's own relay as sent; Password as a secret of the
// settings: the mask keeps it, "" clears it, nil keeps it.
type relayChange struct {
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Security string  `json:"security"`
	Username string  `json:"username"`
	From     string  `json:"from"`
	Password *string `json:"password"`
}

// handlePutSiteMail is PUT /api/sites/{id}/mail (admin).
func (s *Server) handlePutSiteMail(w http.ResponseWriter, r *http.Request, c *call) {
	var req siteMailChange
	if !decodeOr400(w, r, &req) {
		return
	}
	if c.Site.StagingOf != 0 {
		respondErr(w, http.StatusBadRequest, "a staging copy sends no mail")
		return
	}
	fields := map[string]string{}
	if req.From != nil && *req.From != "" && !plainAddress(*req.From) {
		fields["from"] = "from must be one email address"
	}
	if req.DKIMSelector != nil && *req.DKIMSelector != "" && !dkimSelectorRe.MatchString(*req.DKIMSelector) {
		fields["dkim_selector"] = "dkim_selector must be a DNS label such as s1 or google"
	}
	var relay *relayChange
	dropRelay := false
	switch raw := bytes.TrimSpace(req.Relay); {
	case len(raw) == 0:
	case string(raw) == "null":
		dropRelay = true
	default:
		relay = &relayChange{}
		if err := json.Unmarshal(raw, relay); err != nil {
			respondErr(w, http.StatusBadRequest, "malformed request")
			return
		}
		if relay.Host == "" {
			fields["relay.host"] = "the relay needs a host"
		} else if err := checkMailHost(relay.Host); err != nil {
			fields["relay.host"] = "host " + err.Error()
		}
		if relay.Port < 0 || relay.Port > 65535 {
			fields["relay.port"] = "port must be 1–65535, or 0 for the security's port"
		}
		if err := checkMailSecurity(relay.Security); err != nil {
			fields["relay.security"] = "security " + err.Error()
		}
		if relay.From != "" && !plainAddress(relay.From) {
			fields["relay.from"] = "the relay's from must be one email address"
		}
	}
	for _, name := range []string{"from", "dkim_selector", "relay.host", "relay.port", "relay.security", "relay.from"} {
		if m, ok := fields[name]; ok {
			replyFailure(w, &problem{kind: probInvalid, msg: m, fields: fields})
			return
		}
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if old := cur.Config.Mail.Relay; relay != nil && old != nil && c.ViaToken() &&
		(relay.Password == nil || *relay.Password == secretMask) &&
		(relay.Host != old.Host || relay.Port != old.Port || relay.Security != old.Security || relay.Username != old.Username) {
		// The stored password would go wherever the token says (SPEC.md §14).
		respondErr(w, http.StatusBadRequest, "send the relay's password again to change where it is sent")
		return
	}
	change := func(cfg *state.SiteConfig) {
		m := &cfg.Mail
		if req.Off != nil {
			m.Off = *req.Off
		}
		if req.From != nil {
			m.From = *req.From
		}
		if req.DKIMSelector != nil {
			m.DKIMSelector = *req.DKIMSelector
		}
		switch {
		case dropRelay:
			m.Relay = nil
		case relay != nil:
			from := relay.From
			if from == "" {
				// The relay's sender is the site's when none is named.
				from = m.From
			}
			m.Relay = &state.MailRelay{Host: relay.Host, Port: relay.Port, Security: relay.Security, Username: relay.Username, From: from}
		}
	}
	next := cur
	change(&next.Config)
	// The pool carries whether mail() reaches the gate and the sender:
	// those render the site, the rest is the panel's alone.
	if next.Config.Mail.Off != cur.Config.Mail.Off || next.Config.Mail.From != cur.Config.Mail.From {
		if !s.applyWP(w, r, cur, change) {
			return
		}
	} else if _, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		change(&st.Config)
		return nil
	}); err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	secret := state.SiteSecret(cur.ID, state.SiteSecretMailPassword)
	switch {
	case dropRelay:
		s.logIfErr(s.Store.SetSecret(secret, ""), "SetSecret")
	case relay != nil && relay.Password != nil && *relay.Password != secretMask:
		if err := s.Store.SetSecret(secret, *relay.Password); err != nil {
			internalError(w, "SetSecret", err, "")
			return
		}
	}
	site, err := s.Store.GetSite(cur.ID)
	if err != nil {
		internalError(w, "GetSite", err, "")
		return
	}
	m := site.Config.Mail
	where := "server"
	if m.Relay != nil {
		where = m.Relay.Host
	}
	s.audit(c.Actor(), "mail.site", site.Domain, fmt.Sprintf("off=%t from=%s relay=%s", m.Off, m.From, where))
	v, err := s.siteMail(site)
	if err != nil {
		internalError(w, "siteMail", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// mailRecordCheck is one record of the checklist.
type mailRecordCheck struct {
	Found  bool   `json:"found"`
	Record string `json:"record,omitempty"`
}

// mailChecks is GET /api/sites/{id}/mail/checks.
type mailChecks struct {
	Domain string          `json:"domain"`
	SPF    mailRecordCheck `json:"spf"`
	DMARC  mailRecordCheck `json:"dmarc"`
	DKIM   struct {
		Selector string `json:"selector"`
		Found    bool   `json:"found"`
	} `json:"dkim"`
}

// lookupTXT is the checklist's TXT lookup, 3 s at most.
func (s *Server) lookupTXT(ctx context.Context, name string) []string {
	ctx, cancel := context.WithTimeout(ctx, mailCheckTimeout)
	defer cancel()
	look := s.mailTXT
	if look == nil {
		look = net.DefaultResolver.LookupTXT
	}
	recs, err := look(ctx, name)
	if err != nil {
		return nil
	}
	return recs
}

// firstWith is the first record that starts with prefix (case aside).
func firstWith(recs []string, prefix string) mailRecordCheck {
	for _, r := range recs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r)), strings.ToLower(prefix)) {
			return mailRecordCheck{Found: true, Record: r}
		}
	}
	return mailRecordCheck{}
}

func (s *Server) handleSiteMailChecks(w http.ResponseWriter, r *http.Request, c *call) {
	site := c.Site
	sender := site.Config.Mail.From
	if sender == "" {
		if rel, _, scope, err := s.relayFor(site); err == nil && scope != "none" {
			sender = rel.From
		}
	}
	domain := site.Domain
	if _, d, ok := strings.Cut(sender, "@"); ok && d != "" {
		domain = strings.ToLower(d)
	}
	var out mailChecks
	out.Domain = domain
	out.SPF = firstWith(s.lookupTXT(r.Context(), domain), "v=spf1")
	out.DMARC = firstWith(s.lookupTXT(r.Context(), "_dmarc."+domain), "v=DMARC1")
	if sel := site.Config.Mail.DKIMSelector; sel != "" {
		out.DKIM.Selector = sel
		out.DKIM.Found = len(s.lookupTXT(r.Context(), sel+"._domainkey."+domain)) > 0
	}
	respond(w, http.StatusOK, out)
}

// testMailReq is the body of the test routes.
type testMailReq struct {
	To string `json:"to"`
}

// sendTestMail sends the test message to to through relay, from from.
func (s *Server) sendTestMail(ctx context.Context, lang string, relay state.MailRelay, password, from, to, where string) error {
	subject := i18n.T(lang, "mail.testSubject")
	body := i18n.T(lang, "mail.testBody", where)
	msg := plainMessage(from, []string{to}, subject, body, time.Now())
	return s.sendMail(ctx, outgoing{relay: relay, password: password, from: from, to: []string{to}, msg: msg})
}

// decodeTestMail reads {to}, or answers 400.
func decodeTestMail(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req testMailReq
	if !decodeOr400(w, r, &req) {
		return "", false
	}
	if !plainAddress(req.To) {
		respondErr(w, http.StatusBadRequest, "to must be one email address")
		return "", false
	}
	return req.To, true
}

func (s *Server) handleSiteMailTest(w http.ResponseWriter, r *http.Request, c *call) {
	to, ok := decodeTestMail(w, r)
	if !ok {
		return
	}
	relay, password, scope, err := s.relayFor(c.Site)
	if err != nil {
		internalError(w, "relayFor", err, "")
		return
	}
	if scope == "none" {
		respondErr(w, http.StatusPreconditionFailed, "no mail relay is configured")
		return
	}
	if err := s.sendTestMail(r.Context(), readerLanguage(r, c), relay, password, siteSender(c.Site, relay), to, c.Site.Domain); err != nil {
		respondErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.audit(c.Actor(), "mail.test", c.Site.Domain, scope)
	respond(w, http.StatusOK, map[string]int{"sent": 1})
}

func (s *Server) handleSettingsMailTest(w http.ResponseWriter, r *http.Request, c *call) {
	to, ok := decodeTestMail(w, r)
	if !ok {
		return
	}
	relay, password, err := s.serverRelay()
	if err != nil {
		internalError(w, "serverRelay", err, "")
		return
	}
	if !relay.Configured() {
		respondErr(w, http.StatusPreconditionFailed, "no mail relay is configured")
		return
	}
	if err := s.sendTestMail(r.Context(), readerLanguage(r, c), relay, password, relay.From, to, heloName()); err != nil {
		respondErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.audit(c.Actor(), "mail.test", "server", "")
	respond(w, http.StatusOK, map[string]int{"sent": 1})
}
