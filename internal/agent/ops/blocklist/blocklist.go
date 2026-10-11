// Package blocklist is the agent's half of the server's block list
// (internal/blocklist, issue #21): it reads the failed sign-ins only root
// sees, sshd's in the journal and the WordPress vhosts' on a socket nginx
// logs to, hands them to the panel, and enforces in netfilter the list the
// panel decides on. SPEC.md in this directory is the contract.
package blocklist

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	bl "github.com/cloudground-it/cloudground/internal/blocklist"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// DefaultSocket is where the vhosts log failed sign-ins (SPEC §1.2,
// internal/engine SPEC §3.1).
const DefaultSocket = "/run/cloudground/auth-log.sock"

// maxPending bounds the (address, source) pairs kept between two reads
// (SPEC §1.3).
const maxPending = 10000

// Ops reads failures and enforces the block list.
type Ops struct {
	Exec host.Exec
	// Socket is the vhosts' failure log; "" is DefaultSocket.
	Socket string
	// NginxUser is the user nginx's workers run as ("" is www-data); its
	// group may write to the socket and its uid may send on it.
	NginxUser string
	// LookupUser resolves NginxUser to its uid and gid; nil is the user
	// database.
	LookupUser func(name string) (uid, gid int, err error)
	// LocalAddrs are the machine's own addresses; nil reads the interfaces.
	LocalAddrs func() []netip.Addr
	Log        *slog.Logger
	// The country data (countries.go): its directory ("" is
	// DefaultGeoDir), the URL of a month's file and the client that
	// fetches it (nil: DB-IP's, and a client with a 2-minute timeout).
	GeoDir string
	GeoURL func(month string) string
	HTTP   *http.Client

	mu      sync.Mutex
	pending map[pendingKey]int
	// own caches localAddrs for a minute: a flood of failures would
	// otherwise read the interfaces for each.
	own   []netip.Addr
	ownAt time.Time

	sshWatch, nginxWatch atomic.Bool

	// geoMu holds one read or download of the country data at a time;
	// geoCache is the last answer, for geoCacheKey (the file's time and
	// the codes).
	geoMu       sync.Mutex
	geoCacheKey string
	geoCache    []netip.Prefix
}

type pendingKey struct {
	addr   netip.Addr
	source string
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.BlocklistFailures, func(context.Context, agentrpc.None) (rpc.BlocklistFailuresResult, error) {
		return o.Failures(), nil
	})
	agentrpc.Handle(s, agentrpc.BlocklistApply, o.Apply)
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Ops) localAddrs() []netip.Addr {
	if o.LocalAddrs != nil {
		return o.LocalAddrs()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if time.Since(o.ownAt) < time.Minute {
		return o.own
	}
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr().Unmap())
		}
	}
	o.own, o.ownAt = out, time.Now()
	return out
}

// record keeps count failures of a from source (SPEC §1.3).
func (o *Ops) record(a netip.Addr, source string, count int) {
	a = a.Unmap()
	if (bl.Exempt{Own: o.localAddrs()}).Has(a) {
		return
	}
	k := pendingKey{a, source}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == nil {
		o.pending = map[pendingKey]int{}
	}
	if _, ok := o.pending[k]; !ok && len(o.pending) >= maxPending {
		return
	}
	o.pending[k] = min(o.pending[k]+count, 1<<20)
}

// Failures returns what was kept since the last call and empties it
// (SPEC §2).
func (o *Ops) Failures() rpc.BlocklistFailuresResult {
	o.mu.Lock()
	taken := o.pending
	o.pending = nil
	o.mu.Unlock()
	out := rpc.BlocklistFailuresResult{Failures: []rpc.BlocklistFailure{}, SSHWatch: o.sshWatch.Load(), NginxWatch: o.nginxWatch.Load()}
	for k, n := range taken {
		out.Failures = append(out.Failures, rpc.BlocklistFailure{Address: k.addr.String(), Source: k.source, Count: n})
	}
	slices.SortFunc(out.Failures, func(a, b rpc.BlocklistFailure) int {
		return cmp.Or(netip.MustParseAddr(a.Address).Compare(netip.MustParseAddr(b.Address)), strings.Compare(a.Source, b.Source))
	})
	return out
}

// Apply replaces the netfilter table with the list (SPEC §3).
func (o *Ops) Apply(ctx context.Context, p rpc.BlocklistApplyParams) (rpc.BlocklistApplyResult, error) {
	parse := func(list []string) ([]netip.Prefix, error) {
		out := make([]netip.Prefix, 0, len(list))
		for _, s := range list {
			pfx, err := bl.ParsePrefix(s)
			if err != nil {
				return nil, agentrpc.Invalid("%s", err.Error())
			}
			out = append(out, pfx)
		}
		return out, nil
	}
	block, err := parse(p.Block)
	if err != nil {
		return rpc.BlocklistApplyResult{}, err
	}
	for _, pfx := range block {
		if err := bl.CheckBlockWidth(pfx); err != nil {
			return rpc.BlocklistApplyResult{}, agentrpc.Invalid("%s", err.Error())
		}
	}
	allow, err := parse(p.Allow)
	if err != nil {
		return rpc.BlocklistApplyResult{}, err
	}
	block, allow = bl.Minimal(block), bl.Minimal(allow)
	_, err = o.Exec.Run(ctx, host.Cmd{Name: "nft", Args: []string{"-f", "-"}, Stdin: strings.NewReader(bl.NftScript(block, allow)), Timeout: time.Minute})
	if errors.Is(err, exec.ErrNotFound) {
		return rpc.BlocklistApplyResult{}, agentrpc.Wrap(agentrpc.CodeUnavailable, "nftables is not installed on this server: apt install nftables", err)
	}
	if err != nil {
		return rpc.BlocklistApplyResult{}, fmt.Errorf("nft: %w", err)
	}
	return rpc.BlocklistApplyResult{Blocked: len(block), Allowed: len(allow)}, nil
}

// Serve runs both readers until ctx ends (SPEC §1).
func (o *Ops) Serve(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); o.keep(ctx, "sshd journal", &o.sshWatch, o.followJournal) }()
	go func() { defer wg.Done(); o.keep(ctx, "failure log socket", &o.nginxWatch, o.listen) }()
	wg.Wait()
}

// keep runs one reader, again after each failure: 5 s, doubling to a
// minute.
func (o *Ops) keep(ctx context.Context, what string, up *atomic.Bool, run func(context.Context, func()) error) {
	wait := 5 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := run(ctx, func() { up.Store(true) })
		up.Store(false)
		if time.Since(began) > time.Minute {
			wait = 5 * time.Second // it ran: a new failure, not the same one
		}
		if ctx.Err() != nil {
			return
		}
		o.log().Warn("the block list's reader stopped; it starts again", "reader", what, "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}
