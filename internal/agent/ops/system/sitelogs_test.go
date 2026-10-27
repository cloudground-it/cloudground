package system

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func logSite(domain string) state.Site { return state.Site{ID: 3, Domain: domain} }

func TestListSiteLogs(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.Paths.LogRoot, "docs.example.com")
	for name, body := range map[string]string{
		"error.log": "e", "access.log": "aaa", "access.log.1": "a1", "access.log.2.gz": "a2", "access.log.10.gz": "a10",
		"error.log.1": "e1", "error.log.2.gz": "e2",
		// Not the owner's to download.
		"php-error.log": "p", "cleanup.log": "c", "access.log.bak": "x", "access.log.1.zip": "x", "xaccess.log": "x", "access.log.": "x", "..access.log": "x",
	} {
		write(t, filepath.Join(dir, name), body)
	}
	// A link, even to a log, is not listed; nor is a directory.
	if err := os.Symlink(filepath.Join(dir, "access.log"), filepath.Join(dir, "access.log.3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "error.log.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := f.ListSiteLogs(context.Background(), rpc.SiteLogsParams{Site: logSite("docs.example.com")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range res.Files {
		names = append(names, fmt.Sprintf("%s:%d", l.Name, l.Size))
		if l.Modified.IsZero() || time.Since(l.Modified) > time.Hour {
			t.Errorf("%s: modified %v", l.Name, l.Modified)
		}
	}
	want := "access.log:3 error.log:1 access.log.1:2 access.log.2.gz:2 access.log.10.gz:3 error.log.1:2 error.log.2.gz:2"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}

	// A site with no log directory has no logs, and an invalid domain is refused.
	res, err = f.ListSiteLogs(context.Background(), rpc.SiteLogsParams{Site: logSite("new.example.com")})
	if err != nil || res.Files == nil || len(res.Files) != 0 {
		t.Fatalf("no directory: %+v, %v", res, err)
	}
	if _, err := f.ListSiteLogs(context.Background(), rpc.SiteLogsParams{Site: logSite("../etc")}); codeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a path as the domain: %v", err)
	}
}

func TestReadSiteLogChunk(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.Paths.LogRoot, "docs.example.com")
	body := bytes.Repeat([]byte("0123456789"), 100)
	write(t, filepath.Join(dir, "access.log"), string(body))
	write(t, filepath.Join(f.Paths.LogRoot, "secret.example.com", "access.log"), "other site")
	write(t, filepath.Join(f.Paths.LogRoot, "outside.txt"), "outside")
	ctx := context.Background()
	read := func(name string, off int64, n int) (rpc.SiteLogChunkResult, error) {
		return f.ReadSiteLogChunk(ctx, rpc.SiteLogChunkParams{Site: logSite("docs.example.com"), Name: name, Offset: off, Length: n})
	}

	// A file is read whole, in pieces, and the end is an empty piece.
	var got []byte
	for off := int64(0); ; {
		r, err := read("access.log", off, 300)
		if err != nil || r.Size != int64(len(body)) {
			t.Fatalf("offset %d: %+v, %v", off, r, err)
		}
		if len(r.Data) == 0 {
			break
		}
		got = append(got, r.Data...)
		off += int64(len(r.Data))
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes, not the file", len(got))
	}
	if r, err := read("access.log", 5000, 10); err != nil || len(r.Data) != 0 || r.Data == nil {
		t.Fatalf("past the end: %+v, %v", r, err)
	}

	// Anything that is not a log of this site's directory is refused or not found.
	if err := os.Symlink("../secret.example.com/access.log", filepath.Join(dir, "access.log.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside.txt", filepath.Join(dir, "error.log")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"access.log.1", "error.log"} {
		if r, err := read(name, 0, 100); err == nil {
			t.Errorf("a link %s was read: %q", name, r.Data)
		}
	}
	for _, name := range []string{"../secret.example.com/access.log", "/etc/passwd", "access.log/../../x", "php-error.log", "access.log.1/", "", ".", "..", "access.log\x00.gz"} {
		if _, err := read(name, 0, 100); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%q: err = %v, want invalid", name, err)
		}
	}
	if _, err := read("access.log.9", 0, 100); codeOf(err) != agentrpc.CodeNotFound {
		t.Errorf("a log that is not there: %v", err)
	}
	if _, err := f.ReadSiteLogChunk(ctx, rpc.SiteLogChunkParams{Site: logSite("none.example.com"), Name: "access.log", Length: 10}); codeOf(err) != agentrpc.CodeNotFound {
		t.Errorf("a site with no logs: %v", err)
	}
	if _, err := f.ReadSiteLogChunk(ctx, rpc.SiteLogChunkParams{Site: logSite("../secret.example.com"), Name: "access.log", Length: 10}); codeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("a path as the domain: %v", err)
	}
	// A log directory that is a link is not followed.
	if err := os.Symlink(filepath.Join(f.Paths.LogRoot, "secret.example.com"), filepath.Join(f.Paths.LogRoot, "linked.example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadSiteLogChunk(ctx, rpc.SiteLogChunkParams{Site: logSite("linked.example.com"), Name: "access.log", Length: 10}); err == nil {
		t.Error("a linked log directory was followed")
	}
	if _, err := f.ListSiteLogs(ctx, rpc.SiteLogsParams{Site: logSite("linked.example.com")}); err == nil {
		t.Error("a linked log directory was listed")
	}
}

// A FIFO at a log's name cannot hang the agent.
func TestReadSiteLogChunkFIFO(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.Paths.LogRoot, "docs.example.com")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "access.log"), 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.ReadSiteLogChunk(context.Background(), rpc.SiteLogChunkParams{Site: logSite("docs.example.com"), Name: "access.log", Length: 10})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as a log")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read blocked on a FIFO")
	}
}

func TestSiteLogChunkValidate(t *testing.T) {
	ok := rpc.SiteLogChunkParams{Name: "error.log.3.gz", Offset: 0, Length: rpc.MaxSiteLogChunk}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []rpc.SiteLogChunkParams{
		{Name: "access.log", Length: 0}, {Name: "access.log", Length: rpc.MaxSiteLogChunk + 1}, {Name: "access.log", Length: 1, Offset: -1},
		{Name: "../access.log", Length: 1}, {Name: "access.log/x", Length: 1}, {Name: "syslog", Length: 1},
	} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}
