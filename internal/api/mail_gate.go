package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/mailgate"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/throttle"
)

// The mail gate (SPEC.md §14.2): the Unix socket a site's mail() hands its
// messages to, through `panel-agent sendmail` run as the site's user. The
// site is the one whose system user the kernel says connected; nothing the
// client sends can name another. It is not a route of the panel's: it is
// served on its own socket, by MailRoutes.

// mailPerHour bounds a site's messages through the gate.
const mailPerHour = 200

// mailRateGate is the per-site count, made on first use.
type mailRateGate struct {
	once sync.Once
	w    *throttle.Window
}

func (g *mailRateGate) allow(siteID int64) bool {
	g.once.Do(func() { g.w = throttle.New(mailPerHour, time.Hour) })
	return g.w.Allow(strconv.FormatInt(siteID, 10))
}

// peerKey carries a gate connection's peer uid (or the error reading it).
type peerKey struct{}

type peer struct {
	uid uint32
	err error
}

// MailConnContext is the gate server's ConnContext: the uid of the
// process that connected, from the kernel.
func MailConnContext(ctx context.Context, c net.Conn) context.Context {
	uid, err := agentrpc.PeerUID(c)
	return context.WithValue(ctx, peerKey{}, peer{uid: uid, err: err})
}

// userName is the account name of uid.
func (s *Server) userName(uid uint32) (string, error) {
	if s.gateUser != nil {
		return s.gateUser(uid)
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// gateSite is the site whose system user is the connection's peer.
func (s *Server) gateSite(r *http.Request) (state.Site, error) {
	p, ok := r.Context().Value(peerKey{}).(peer)
	if !ok || p.err != nil {
		return state.Site{}, errors.New("the connecting process is unknown")
	}
	name, err := s.userName(p.uid)
	if err != nil || name == "" {
		return state.Site{}, fmt.Errorf("uid %d has no account", p.uid)
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		return state.Site{}, err
	}
	for _, st := range sites {
		if st.SystemUser == name {
			return st, nil
		}
	}
	return state.Site{}, fmt.Errorf("%s is not a site's user", name)
}

// handleGateSend is POST /send: the message a site's mail() handed over,
// checked and sent through the site's relay (SPEC.md §14.2).
func (s *Server) handleGateSend(w http.ResponseWriter, r *http.Request) {
	site, err := s.gateSite(r)
	if err != nil {
		s.Log.Warn("mail gate: refused a sender", "err", err)
		respondErr(w, http.StatusForbidden, "this user is not a site's")
		return
	}
	refuse := func(status int, msg string) {
		s.Log.Info("mail gate: refused", "site", site.Domain, "status", status, "reason", msg)
		respondErr(w, status, msg)
	}
	switch {
	case site.StagingOf != 0:
		refuse(http.StatusForbidden, "a staging copy sends no mail")
		return
	case site.Config.Mail.Off:
		refuse(http.StatusForbidden, "mail is turned off for this site")
		return
	}
	relay, password, scope, err := s.relayFor(site)
	if err != nil {
		s.logIfErr(err, "relayFor")
		refuse(http.StatusInternalServerError, "the mail relay's settings cannot be read")
		return
	}
	if scope == "none" {
		refuse(http.StatusPreconditionFailed, "no mail relay is configured")
		return
	}
	if !s.mailRate.allow(site.ID) {
		refuse(http.StatusTooManyRequests, "too many messages from this site; try again later")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mailgate.MaxMessage))
	if err != nil {
		refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("the message is larger than %d MB", mailgate.MaxMessage>>20))
		return
	}
	from := siteSender(site, relay)
	var own func(string) bool // the site's own relay: whatever it accepts
	if scope == "server" {
		own = func(addr string) bool { return strings.EqualFold(addr, from) || siteNamesAddress(site, addr) }
	}
	q := r.URL.Query()
	msg, to, err := prepareMessage(raw, q["to"], q.Get("t") == "1", from, own, time.Now())
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.sendMail(r.Context(), outgoing{relay: relay, password: password, from: from, to: to, msg: msg}); err != nil {
		s.Log.Warn("mail gate: the relay refused", "site", site.Domain, "recipients", len(to), "err", err)
		respondErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("mail gate: sent", "site", site.Domain, "recipients", len(to), "relay", scope)
	respond(w, http.StatusOK, map[string]int{"sent": len(to)})
}
