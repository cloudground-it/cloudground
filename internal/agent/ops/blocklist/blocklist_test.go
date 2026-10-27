package blocklist

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

var own = []netip.Addr{netip.MustParseAddr("192.0.2.10")}

func newOps(fe *host.FakeExec) *Ops {
	return &Ops{Exec: fe, LocalAddrs: func() []netip.Addr { return own }}
}

// SPEC §1.1: only root's entries in the SSH unit count; a look-alike line
// from a user, or from root in another unit, does not.
func TestJournalTrustedFieldsOnly(t *testing.T) {
	stream := strings.Join([]string{
		`{"_UID":"0","_SYSTEMD_UNIT":"ssh.service","MESSAGE":"Failed password for root from 203.0.113.9 port 1 ssh2"}`,
		`{"_UID":"0","_SYSTEMD_UNIT":"sshd.service","MESSAGE":"Invalid user oracle from 203.0.113.9 port 2"}`,
		// The message as an array of bytes (not valid UTF-8 elsewhere in it).
		`{"_UID":"0","_SYSTEMD_UNIT":"ssh.service","MESSAGE":[73,110,118,97,108,105,100,32,117,115,101,114,32,120,32,102,114,111,109,32,50,48,51,46,48,46,49,49,51,46,57,32,112,111,114,116,32,51]}`,
		`{"_UID":"1001","_SYSTEMD_UNIT":"ssh.service","MESSAGE":"Failed password for root from 198.51.100.1 port 1 ssh2"}`,
		`{"_UID":"0","_SYSTEMD_UNIT":"cron.service","MESSAGE":"Failed password for root from 198.51.100.2 port 1 ssh2"}`,
		`{"_UID":"0","MESSAGE":"Failed password for root from 198.51.100.3 port 1 ssh2"}`,
		`{"_UID":"0","_SYSTEMD_UNIT":"ssh.service","MESSAGE":"Failed password for root from 127.0.0.1 port 1 ssh2"}`,
		`{"_UID":"0","_SYSTEMD_UNIT":"ssh.service","MESSAGE":"Failed password for root from 192.0.2.10 port 1 ssh2"}`,
		`not json`,
	}, "\n") + "\n"
	fe := &host.FakeExec{Respond: func(c host.FakeCall) host.FakeReply { return host.FakeReply{Stdout: stream} }}
	o := newOps(fe)
	if err := o.followJournal(context.Background(), func() {}); err == nil {
		t.Fatal("a journalctl that ended was not an error (the reader would not restart)")
	}
	got := o.Failures()
	if len(got.Failures) != 1 || got.Failures[0] != (rpc.BlocklistFailure{Address: "203.0.113.9", Source: "ssh", Count: 3}) {
		t.Fatalf("failures = %+v", got.Failures)
	}
	c := fe.Calls()[0]
	if c.Line() != "journalctl -f -n 0 -q -o json -u ssh.service -u sshd.service" {
		t.Fatalf("ran %s", c.Line())
	}
	if again := o.Failures(); len(again.Failures) != 0 {
		t.Fatalf("a read did not empty the failures: %+v", again.Failures)
	}
}

// SPEC §1.2: a datagram nginx (here root) sent over the real socket counts;
// a malformed one does not; the socket is 0620.
func TestSocketReader(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PASSCRED is Linux's")
	}
	dir := t.TempDir()
	o := newOps(&host.FakeExec{})
	o.Socket = filepath.Join(dir, "auth.sock")
	o.LookupUser = func(string) (int, int, error) { return os.Getuid(), os.Getgid(), nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- o.listen(ctx, func() { close(up) }) }()
	select {
	case <-up:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("the socket did not open")
	}
	if fi, err := os.Stat(o.Socket); err != nil || fi.Mode().Perm() != 0o620 {
		t.Fatalf("socket mode %v %v", fi, err)
	}
	c, err := net.Dial("unixgram", o.Socket)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=2",
		"<190>Oct 11 21:00:00 cloudground: 203.0.113.9 failed=1",
		"<190>Oct 11 21:00:00 cloudground: 127.0.0.1 failed=1",
		"garbage",
	} {
		if _, err := c.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	// The lines may arrive over several reads, and a read empties: add up
	// what each read gives until the three failures are in.
	deadline := time.Now().Add(5 * time.Second)
	total := 0
	for total < 3 && time.Now().Before(deadline) {
		for _, f := range o.Failures().Failures {
			if f.Address != "203.0.113.9" || f.Source != "wordpress" {
				t.Fatalf("unexpected failure %+v", f)
			}
			total += f.Count
		}
		if total < 3 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Nothing more comes: the loopback line and the garbage never count.
	time.Sleep(100 * time.Millisecond)
	for _, f := range o.Failures().Failures {
		t.Fatalf("unexpected failure %+v", f)
	}
	if total != 3 {
		t.Fatalf("counted %d failures, want 3", total)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("listen after cancel: %v", err)
	}
}

// SPEC §1.2: only root's and nginx's datagrams count.
func TestSentBy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_PASSCRED is Linux's")
	}
	if sentBy(nil, 33) {
		t.Fatal("a datagram without credentials counted")
	}
}

// SPEC §1.3: pairs are bounded between reads.
func TestPendingBound(t *testing.T) {
	o := newOps(&host.FakeExec{})
	for i := range maxPending + 5 {
		o.record(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), "ssh", 1)
	}
	if n := len(o.Failures().Failures); n != maxPending {
		t.Fatalf("kept %d pairs, want %d", n, maxPending)
	}
}

// SPEC §3: the table is replaced with the minimal lists; a refused entry,
// a block that is too wide, and nft missing are errors.
func TestApply(t *testing.T) {
	fe := &host.FakeExec{}
	o := newOps(fe)
	res, err := o.Apply(context.Background(), rpc.BlocklistApplyParams{
		Block: []string{"203.0.113.0/24", "203.0.113.9", "2001:db8::/64"},
		Allow: []string{"198.51.100.7"},
	})
	if err != nil || res != (rpc.BlocklistApplyResult{Blocked: 2, Allowed: 1}) {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	c := fe.Calls()[0]
	if c.Line() != "nft -f -" || !strings.Contains(c.Stdin, "elements = { 203.0.113.0/24 }") || !strings.Contains(c.Stdin, "elements = { 198.51.100.7/32 }") {
		t.Fatalf("ran %s with:\n%s", c.Line(), c.Stdin)
	}
	for _, p := range []rpc.BlocklistApplyParams{{Block: []string{"nope"}}, {Block: []string{"0.0.0.0/0"}}, {Allow: []string{"x/1"}}} {
		if _, err := o.Apply(context.Background(), p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v = %v", p, err)
		}
	}
	missing := newOps(&host.FakeExec{Respond: func(host.FakeCall) host.FakeReply { return host.FakeReply{Err: exec.ErrNotFound} }})
	if _, err := missing.Apply(context.Background(), rpc.BlocklistApplyParams{}); agentrpc.CodeOf(err) != agentrpc.CodeUnavailable {
		t.Fatalf("nft missing = %v", err)
	}
	refused := newOps(&host.FakeExec{Respond: func(host.FakeCall) host.FakeReply { return host.FakeReply{Code: 1, Stderr: "syntax"} }})
	if _, err := refused.Apply(context.Background(), rpc.BlocklistApplyParams{}); err == nil {
		t.Fatal("an nft failure was not an error")
	}
}

// The sites behind Cloudflare get the list after netfilter (SPEC §3).
func TestApplyGivesNginxTheList(t *testing.T) {
	o := newOps(&host.FakeExec{})
	var got []string
	o.Banned = func(_ context.Context, block, allow []netip.Prefix) error {
		got = append(got, fmt.Sprint(block), fmt.Sprint(allow))
		return nil
	}
	if _, err := o.Apply(context.Background(), rpc.BlocklistApplyParams{Block: []string{"203.0.113.0/24", "203.0.113.9"}, Allow: []string{"198.51.100.7"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "[203.0.113.0/24] [198.51.100.7/32]" {
		t.Fatalf("nginx got %v", got)
	}
	o.Banned = func(context.Context, []netip.Prefix, []netip.Prefix) error { return errors.New("[emerg] test") }
	if _, err := o.Apply(context.Background(), rpc.BlocklistApplyParams{}); err == nil || !strings.HasPrefix(err.Error(), "nginx: ") {
		t.Fatalf("an nginx failure = %v", err)
	}
}
