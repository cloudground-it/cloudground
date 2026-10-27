package traffic

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func newOps(t *testing.T) (*Ops, string) {
	t.Helper()
	root := t.TempDir()
	o := &Ops{LogRoot: filepath.Join(root, "logs"), Dir: filepath.Join(root, "traffic"), Now: func() time.Time { return now }}
	if err := os.MkdirAll(filepath.Join(o.LogRoot, "shop.example.com"), 0o755); err != nil {
		t.Fatal(err)
	}
	return o, filepath.Join(o.LogRoot, "shop.example.com", "access.log")
}

// view is one page view from addr at minute m of 10:00.
func view(addr, path string, m int) string {
	return fmt.Sprintf(`%s - shop.example.com [27/Oct/2026:10:%02d:00 +0000] "GET %s HTTP/2.0" 200 100 0.002 cache:HIT "Mozilla/5.0 Firefox/140.0"`+"\n", addr, m, path)
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

var site = rpc.TrafficTarget{ID: 7, Domain: "shop.example.com"}

func collect(t *testing.T, o *Ops, sites ...rpc.TrafficTarget) rpc.TrafficCollected {
	t.Helper()
	res, err := o.Collect(context.Background(), rpc.TrafficCollectParams{Sites: sites})
	if err != nil || len(res.Sites) != len(sites) {
		t.Fatalf("collect: %+v %v", res, err)
	}
	if len(res.Sites) == 0 {
		return rpc.TrafficCollected{}
	}
	return res.Sites[0]
}

func pageviews(t *testing.T, o *Ops) int64 {
	t.Helper()
	st, err := o.Stats(context.Background(), rpc.TrafficStatsParams{TrafficTarget: site, Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	return st.Totals.Pageviews
}

func TestCollectAcrossRounds(t *testing.T) {
	o, log := newOps(t)
	// Nothing logged yet: no error.
	if row := collect(t, o, site); row.Error != "" || row.Lines != 0 {
		t.Fatalf("no log: %+v", row)
	}
	appendTo(t, log, view("203.0.113.1", "/", 1)+view("203.0.113.2", "/a/", 2)+"garbage line\n")
	// A line still being written waits for the next round.
	appendTo(t, log, `203.0.113.3 - shop.example.com [27/Oct/2026:10:03:00 +0000] "GET /b/`)
	row := collect(t, o, site)
	if row.Lines != 2 || row.Skipped != 1 || pageviews(t, o) != 2 {
		t.Fatalf("first round %+v, %d views", row, pageviews(t, o))
	}
	appendTo(t, log, ` HTTP/2.0" 200 100 0.002 cache:HIT "Mozilla/5.0 Firefox/140.0"`+"\n")
	if row := collect(t, o, site); row.Lines != 1 || pageviews(t, o) != 3 {
		t.Fatalf("second round %+v", row)
	}
	// Again: nothing new, nothing counted twice.
	if collect(t, o, site); pageviews(t, o) != 3 {
		t.Fatal("counted twice")
	}
	st, _ := o.Stats(context.Background(), rpc.TrafficStatsParams{TrafficTarget: site, Days: 7})
	if st.CollectedAt == nil || st.Totals.Visitors != 3 {
		t.Fatalf("stats %+v", st)
	}
	// No address in the state file.
	b, _ := os.ReadFile(o.file(site.ID))
	if strings.Contains(string(b), "203.0.113") {
		t.Fatalf("an address is stored: %s", b)
	}
	if fi, _ := os.Stat(o.file(site.ID)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestBoundedRead(t *testing.T) {
	o, log := newOps(t)
	o.MaxRead = int64(len(view("203.0.113.1", "/p0/", 0))) * 2
	for i := range 5 {
		appendTo(t, log, view("203.0.113.1", fmt.Sprintf("/p%d/", i), i))
	}
	if row := collect(t, o, site); row.Lines != 2 {
		t.Fatalf("a bounded round read %d lines", row.Lines)
	}
	collect(t, o, site)
	collect(t, o, site)
	if pageviews(t, o) != 5 {
		t.Fatalf("%d views after three rounds", pageviews(t, o))
	}
	// One line longer than the whole read is skipped, not stuck on.
	appendTo(t, log, strings.Repeat("x", int(o.MaxRead)+10)+"\n"+view("203.0.113.1", "/after/", 9))
	for range 4 {
		collect(t, o, site)
	}
	if pageviews(t, o) != 6 {
		t.Fatalf("%d views past a huge line", pageviews(t, o))
	}
}

// logrotate's copytruncate: the old log's tail is in access.log.1 and the
// log starts again; a round in between loses nothing.
func TestCopyTruncate(t *testing.T) {
	o, log := newOps(t)
	appendTo(t, log, view("203.0.113.1", "/1/", 1))
	collect(t, o, site)
	appendTo(t, log, view("203.0.113.1", "/2/", 2))
	old, _ := os.ReadFile(log)
	if err := os.WriteFile(log+".1", old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(log, 0); err != nil {
		t.Fatal(err)
	}
	// Written past the old offset before the round: a size check alone would miss it.
	appendTo(t, log, view("198.51.100.1", "/3/", 3)+view("198.51.100.1", "/4/", 4))
	collect(t, o, site)
	if got := pageviews(t, o); got != 4 {
		t.Fatalf("%d views across the rotation, want 4", got)
	}
}

func TestRenameRotation(t *testing.T) {
	o, log := newOps(t)
	appendTo(t, log, view("203.0.113.1", "/1/", 1))
	collect(t, o, site)
	appendTo(t, log, view("203.0.113.1", "/2/", 2))
	if err := os.Rename(log, log+".1"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, log, view("203.0.113.1", "/3/", 3))
	collect(t, o, site)
	if got := pageviews(t, o); got != 3 {
		t.Fatalf("%d views across a rename, want 3", got)
	}
}

func TestLinksAndDroppedSites(t *testing.T) {
	o, log := newOps(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(view("203.0.113.1", "/", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, log); err != nil {
		t.Fatal(err)
	}
	if row := collect(t, o, site); row.Error == "" || row.Lines != 0 {
		t.Fatalf("a link was followed: %+v", row)
	}
	other := rpc.TrafficTarget{ID: 8, Domain: "other.example.com"}
	if err := os.MkdirAll(filepath.Join(o.LogRoot, other.Domain), 0o755); err != nil {
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(o.LogRoot, other.Domain, "access.log"), view("203.0.113.1", "/", 1))
	collect(t, o, site, other)
	if _, err := os.Stat(o.file(8)); err != nil {
		t.Fatal("no state for the other site")
	}
	collect(t, o, site)
	if _, err := os.Stat(o.file(8)); !os.IsNotExist(err) {
		t.Fatal("a dropped site's statistics stayed")
	}
}

func TestParamsChecked(t *testing.T) {
	o, _ := newOps(t)
	for _, p := range []rpc.TrafficCollectParams{
		{Sites: []rpc.TrafficTarget{{ID: 0, Domain: "a.example.com"}}},
		{Sites: []rpc.TrafficTarget{{ID: 1, Domain: "../etc"}}},
		{Sites: []rpc.TrafficTarget{site, site}},
	} {
		if _, err := o.Collect(context.Background(), p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v = %v", p, err)
		}
	}
	if _, err := o.Stats(context.Background(), rpc.TrafficStatsParams{TrafficTarget: site, Days: 91}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("91 days = %v", err)
	}
}
