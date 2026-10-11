package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The server's block list (SPEC.md §7.4, issue #21; internal/blocklist
// SPEC): addresses blocked by hand or for repeated failed sign-ins at SSH,
// WordPress and the panel, the allowlist that is never blocked, and the
// watcher that counts the agent's failures and keeps netfilter in step
// with the list.

const (
	// defaultBlocklistEvery is how often the watcher reads the agent's
	// failures and checks the list.
	defaultBlocklistEvery = 10 * time.Second
	// blocklistRefresh is how old an apply may be before the same list is
	// sent again: netfilter's copy is gone after a reboot, and nothing else
	// would say so.
	blocklistRefresh = 10 * time.Minute
	// maxBlockMinutes bounds a block made by hand with an end (a year).
	maxBlockMinutes = 525600
	// maxBlockNote bounds an entry's note.
	maxBlockNote = 200
)

// blocklistState is the panel's side of the list: the failure counts and
// what the agent last took.
type blocklistState struct {
	tracker *blocklist.Tracker
	kick    chan struct{}
	// applyMu holds one read of the list and its apply at a time: two
	// interleaved could leave netfilter with the older list.
	applyMu sync.Mutex

	mu        sync.Mutex
	applied   string // the fingerprint of the list netfilter has
	appliedAt time.Time
	err       string // the last apply's failure; "" after a success
	sshWatch  bool
	wpWatch   bool
}

func (s *Server) initBlocklist() {
	if s.blocks.tracker == nil {
		s.blocks.tracker = blocklist.NewTracker()
	}
	if s.blocks.kick == nil {
		s.blocks.kick = make(chan struct{}, 1)
	}
}

// kickBlocklist asks the watcher for a round now.
func (s *Server) kickBlocklist() {
	select {
	case s.blocks.kick <- struct{}{}:
	default:
	}
}

// ownAddrs are this machine's addresses: never blocked.
func (s *Server) ownAddrs() []netip.Addr {
	if s.localAddrs != nil {
		return s.localAddrs()
	}
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	return out
}

// exempt is what is never blocked: loopback, this machine and the
// allowlist (internal/blocklist SPEC §1).
func (s *Server) exempt(entries []state.BlockEntry) blocklist.Exempt {
	ex := blocklist.Exempt{Own: s.ownAddrs()}
	for _, e := range entries {
		if e.Kind != state.BlockKindAllow {
			continue
		}
		if p, err := blocklist.ParsePrefix(e.Prefix); err == nil {
			ex.Allow = append(ex.Allow, p)
		}
	}
	return ex
}

// signInFailed is the accounts service's report of a wrong password or
// code at the panel's sign-in.
func (s *Server) signInFailed(address string) {
	s.recordFailure(address, blocklist.SourcePanel, 1, time.Now())
}

// recordFailure counts failures of address and blocks its unit once it has
// reached the threshold (internal/blocklist SPEC §3).
func (s *Server) recordFailure(address, source string, count int, now time.Time) {
	a, err := netip.ParseAddr(address)
	if err != nil || a.Zone() != "" {
		return // "unix": the API socket without nginx, nobody to block
	}
	settings, err := s.Store.BlocklistSettings()
	if err != nil {
		s.Log.Error("blocklist settings", "err", err)
		return
	}
	entries, err := s.Store.BlockEntries()
	if err != nil {
		s.Log.Error("blocklist entries", "err", err)
		return
	}
	unit, ban := s.blocks.tracker.Fail(a, count, now, settings, s.exempt(entries))
	if !ban {
		return
	}
	ends := now.Add(time.Duration(settings.BanMinutes) * time.Minute)
	// A block of the same unit that lasts as long or longer stays as it is
	// (a manual block, made permanent, is not shortened).
	if slices.ContainsFunc(entries, func(e state.BlockEntry) bool {
		return e.Kind == state.BlockKindBlock && e.Prefix == unit.String() && (e.ExpiresAt == nil || !e.ExpiresAt.Before(ends))
	}) {
		return
	}
	if _, err := s.Store.PutBlockEntry(state.BlockEntry{Kind: state.BlockKindBlock, Prefix: unit.String(), Source: source,
		Failures: settings.MaxFailures, CreatedAt: now, ExpiresAt: &ends}); err != nil {
		s.Log.Error("blocklist: could not record a block", "address", unit.String(), "err", err)
		return
	}
	s.Log.Info("blocked an address for failed sign-ins", "address", unit.String(), "source", source, "until", ends.UTC().Format(time.RFC3339))
	s.kickBlocklist()
}

// watchBlocklist reads the agent's failures and keeps netfilter in step
// with the list until ctx ends (SPEC.md §7.4).
func (s *Server) watchBlocklist(ctx context.Context) {
	every := s.blocklistEvery
	if every <= 0 {
		every = defaultBlocklistEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s.blocklistRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.blocks.kick:
		}
	}
}

// blocklistRound is one turn of the watcher: ended blocks go, the agent's
// failures are counted, and the list is sent when it changed.
func (s *Server) blocklistRound(ctx context.Context) {
	now := time.Now()
	if _, err := s.Store.PruneBlockEntries(now); err != nil {
		s.Log.Error("blocklist: prune", "err", err)
	}
	var got rpc.BlocklistFailuresResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.BlocklistFailures, agentrpc.None{}, &got); err == nil {
		s.blocks.mu.Lock()
		s.blocks.sshWatch, s.blocks.wpWatch = got.SSHWatch, got.NginxWatch
		s.blocks.mu.Unlock()
		for _, f := range got.Failures {
			s.recordFailure(f.Address, f.Source, f.Count, now)
		}
	}
	s.syncBlocklist(ctx, false)
}

// syncBlocklist sends the list to the agent when it differs from what
// netfilter has, when the last apply is old or failed, or when force is
// set.
func (s *Server) syncBlocklist(ctx context.Context, force bool) error {
	s.blocks.applyMu.Lock()
	defer s.blocks.applyMu.Unlock()
	entries, err := s.Store.BlockEntries()
	if err != nil {
		return err
	}
	var p rpc.BlocklistApplyParams
	now := time.Now()
	for _, e := range entries {
		switch {
		case e.Kind == state.BlockKindAllow:
			p.Allow = append(p.Allow, e.Prefix)
		case e.ExpiresAt == nil || e.ExpiresAt.After(now):
			p.Block = append(p.Block, e.Prefix)
		}
	}
	slices.Sort(p.Block)
	slices.Sort(p.Allow)
	sum := sha256.Sum256([]byte(strings.Join(p.Block, ",") + "|" + strings.Join(p.Allow, ",")))
	fp := hex.EncodeToString(sum[:])
	s.blocks.mu.Lock()
	current := s.blocks.applied == fp && s.blocks.err == "" && now.Sub(s.blocks.appliedAt) < blocklistRefresh
	s.blocks.mu.Unlock()
	if current && !force {
		return nil
	}
	_, err = agentrpc.Call(ctx, s.Agent, agentrpc.BlocklistApply, p)
	s.blocks.mu.Lock()
	defer s.blocks.mu.Unlock()
	if err != nil {
		_, msg := errorReply(err, 0, "could not apply the block list")
		if s.blocks.err != msg {
			s.Log.Warn("the block list is not enforced", "err", err)
		}
		s.blocks.err = msg
		s.blocks.applied = ""
		return err
	}
	s.blocks.applied, s.blocks.appliedAt, s.blocks.err = fp, now, ""
	return nil
}

// blocklistView is GET /api/blocklist.
type blocklistView struct {
	Settings blocklist.Settings `json:"settings"`
	Blocked  []state.BlockEntry `json:"blocked"`
	Allowed  []state.BlockEntry `json:"allowed"`
	// Enforced: netfilter has the list as it is now; Error is why not.
	Enforced  bool       `json:"enforced"`
	Error     string     `json:"error,omitempty"`
	AppliedAt *time.Time `json:"applied_at,omitempty"`
	// Watching: which failures the agent is reading now.
	Watching struct {
		SSH       bool `json:"ssh"`
		WordPress bool `json:"wordpress"`
	} `json:"watching"`
	// YourAddress is the caller's, as the panel sees it: the one to allow.
	YourAddress string `json:"your_address"`
}

func (s *Server) handleGetBlocklist(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.BlocklistSettings()
	if err != nil {
		internalError(w, "BlocklistSettings", err, "could not read the block list")
		return
	}
	entries, err := s.Store.BlockEntries()
	if err != nil {
		internalError(w, "BlockEntries", err, "could not read the block list")
		return
	}
	v := blocklistView{Settings: settings, Blocked: []state.BlockEntry{}, Allowed: []state.BlockEntry{}, YourAddress: clientIP(r)}
	now := time.Now()
	for _, e := range entries {
		switch {
		case e.Kind == state.BlockKindAllow:
			v.Allowed = append(v.Allowed, e)
		case e.ExpiresAt == nil || e.ExpiresAt.After(now):
			v.Blocked = append(v.Blocked, e)
		}
	}
	// The newest block first: the one a person comes to look at.
	slices.SortStableFunc(v.Blocked, func(a, b state.BlockEntry) int { return b.CreatedAt.Compare(a.CreatedAt) })
	s.blocks.mu.Lock()
	v.Enforced = s.blocks.err == "" && !s.blocks.appliedAt.IsZero()
	v.Error = s.blocks.err
	if !s.blocks.appliedAt.IsZero() {
		at := s.blocks.appliedAt.UTC()
		v.AppliedAt = &at
	}
	v.Watching.SSH, v.Watching.WordPress = s.blocks.sshWatch, s.blocks.wpWatch
	s.blocks.mu.Unlock()
	respond(w, http.StatusOK, v)
}

// blockReq is the body of the block list's write routes; DELETE reads
// only Address.
type blockReq struct {
	Address string `json:"address"`
	Note    string `json:"note"`
	// Minutes is how long a block lasts; 0 is until it is removed.
	Minutes int `json:"minutes"`
}

// parseBlockReq checks what both kinds of entry share.
func parseBlockReq(req blockReq) (netip.Prefix, error) {
	p, err := blocklist.ParsePrefix(req.Address)
	if err != nil {
		return netip.Prefix{}, decline(http.StatusBadRequest, "%s", err.Error())
	}
	if len(req.Note) > maxBlockNote || strings.ContainsAny(req.Note, "\r\n\x00") {
		return netip.Prefix{}, decline(http.StatusBadRequest, "the note is one line of at most %d bytes", maxBlockNote)
	}
	return p, nil
}

// handleBlock blocks an address or a network by hand.
func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request, c *call) {
	var req blockReq
	if !decodeOr400(w, r, &req) {
		return
	}
	p, err := parseBlockReq(req)
	if err != nil {
		fail(w, err)
		return
	}
	if err := blocklist.CheckBlockWidth(p); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Minutes < 0 || req.Minutes > maxBlockMinutes {
		respondErr(w, http.StatusBadRequest, "minutes must be 0 (until removed) to 525600")
		return
	}
	if me, err := netip.ParseAddr(clientIP(r)); err == nil && p.Contains(me.Unmap()) {
		respondErr(w, http.StatusBadRequest, p.String()+" holds your own address ("+me.Unmap().String()+"): you would lock yourself out")
		return
	}
	entries, err := s.Store.BlockEntries()
	if err != nil {
		internalError(w, "BlockEntries", err, "could not read the block list")
		return
	}
	if s.exempt(entries).Overlaps(p) {
		respondErr(w, http.StatusBadRequest, p.String()+" holds this server, loopback or an allowed address: those are never blocked")
		return
	}
	e := state.BlockEntry{Kind: state.BlockKindBlock, Prefix: p.String(), Source: blocklist.SourceManual, Note: req.Note, CreatedAt: time.Now()}
	if req.Minutes > 0 {
		ends := e.CreatedAt.Add(time.Duration(req.Minutes) * time.Minute)
		e.ExpiresAt = &ends
	}
	if _, err := s.Store.PutBlockEntry(e); err != nil {
		internalError(w, "PutBlockEntry", err, "could not save the block")
		return
	}
	detail := "until removed"
	if e.ExpiresAt != nil {
		detail = "until " + e.ExpiresAt.UTC().Format(time.RFC3339)
	}
	s.audit(c.Actor(), "blocklist.block", p.String(), detail)
	s.applyNow(w, r)
}

// handleUnblock removes a block, made by hand or for failures.
func (s *Server) handleUnblock(w http.ResponseWriter, r *http.Request, c *call) {
	s.removeEntry(w, r, c, state.BlockKindBlock, "that address is not blocked", "blocklist.unblock")
}

// handleAllow adds an address or a network to the allowlist: never blocked,
// and its automatic blocks end now.
func (s *Server) handleAllow(w http.ResponseWriter, r *http.Request, c *call) {
	var req blockReq
	if !decodeOr400(w, r, &req) {
		return
	}
	p, err := parseBlockReq(req)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := s.Store.PutBlockEntry(state.BlockEntry{Kind: state.BlockKindAllow, Prefix: p.String(), Source: blocklist.SourceManual, Note: req.Note, CreatedAt: time.Now()}); err != nil {
		internalError(w, "PutBlockEntry", err, "could not save the address")
		return
	}
	entries, err := s.Store.BlockEntries()
	if err == nil {
		for _, e := range entries {
			bp, perr := blocklist.ParsePrefix(e.Prefix)
			if e.Kind == state.BlockKindBlock && e.Source != blocklist.SourceManual && perr == nil && p.Overlaps(bp) {
				_, _ = s.Store.DeleteBlockEntry(state.BlockKindBlock, e.Prefix)
				s.blocks.tracker.Forget(bp)
			}
		}
	}
	s.audit(c.Actor(), "blocklist.allow", p.String(), req.Note)
	s.applyNow(w, r)
}

// handleDisallow takes an entry off the allowlist.
func (s *Server) handleDisallow(w http.ResponseWriter, r *http.Request, c *call) {
	s.removeEntry(w, r, c, state.BlockKindAllow, "that address is not on the allowlist", "blocklist.disallow")
}

// removeEntry removes the row of kind for the body's address.
func (s *Server) removeEntry(w http.ResponseWriter, r *http.Request, c *call, kind, missing, action string) {
	var req blockReq
	if !decodeOr400(w, r, &req) {
		return
	}
	p, err := parseBlockReq(req)
	if err != nil {
		fail(w, err)
		return
	}
	ok, err := s.Store.DeleteBlockEntry(kind, p.String())
	if err != nil {
		internalError(w, "DeleteBlockEntry", err, "could not change the block list")
		return
	}
	if !ok {
		respondErr(w, http.StatusNotFound, missing)
		return
	}
	s.blocks.tracker.Forget(p)
	s.audit(c.Actor(), action, p.String(), "")
	s.applyNow(w, r)
}

// applyNow sends the changed list to the agent and answers the list. A
// list the agent could not take is still saved: the answer says it is not
// enforced, and the watcher tries again.
func (s *Server) applyNow(w http.ResponseWriter, r *http.Request) {
	_ = s.syncBlocklist(r.Context(), true)
	s.handleGetBlocklist(w, r)
}

// handlePutBlocklistSettings changes the automatic blocking's policy.
func (s *Server) handlePutBlocklistSettings(w http.ResponseWriter, r *http.Request, c *call) {
	var req blocklist.Settings
	if !decodeOr400(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.SetBlocklistSettings(req); err != nil {
		internalError(w, "SetBlocklistSettings", err, "could not save the settings")
		return
	}
	onOff := "off"
	if req.Enabled {
		onOff = "on"
	}
	s.audit(c.Actor(), "blocklist.settings", "automatic blocking "+onOff,
		fmt.Sprintf("%d failures in %d min, blocked %d min", req.MaxFailures, req.WindowMinutes, req.BanMinutes))
	s.handleGetBlocklist(w, r)
}
