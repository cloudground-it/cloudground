package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The tail reads only the end of the file, survives a line longer than any
// buffer, and does not follow a symlink at the log's name.
func TestTailFileReadsTheEndOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "access.log")
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", 40))
	}
	b.WriteString(strings.Repeat("y", 3<<20) + "\n")
	b.WriteString("last line\n")
	write(t, p, b.String())
	out, err := tailFile(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[2] != "last line" || !strings.HasSuffix(lines[1], lineCut) || len(lines[1]) > 9<<10 {
		t.Fatalf("tail = %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	// The window opened mid-line: the fragment is not shown as a line.
	all, _ := tailFile(p, 1<<30)
	first, _, _ := strings.Cut(all, "\n")
	var n int
	if _, err := fmt.Sscanf(first, "line %d", &n); err != nil || first != fmt.Sprintf("line %d %s", n, strings.Repeat("x", 40)) {
		t.Fatalf("first line of the window = %.60q", first)
	}

	secret := filepath.Join(dir, "secret")
	write(t, secret, "root only")
	link := filepath.Join(dir, "php-error.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if out, err := tailFile(link, 10); err == nil || strings.Contains(out, "root only") {
		t.Fatalf("the tail followed a symlink: %q %v", out, err)
	}
	if out, err := tailFile(filepath.Join(dir, "missing.log"), 10); err != nil || out != noLogYet {
		t.Fatalf("missing log = %q %v", out, err)
	}
	if out, err := tailFile(secret, 10); err != nil || out != "root only" {
		t.Fatalf("short log = %q %v", out, err)
	}
}

// A FIFO at a log's name is refused at once, not waited on.
func TestTailFileDoesNotBlockOnAFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "error.log")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { _, err := tailFile(p, 10); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as a log")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tailFile blocked on a FIFO")
	}
}

func TestTailLogSources(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.Paths.NginxLogDir, "error.log"), "nginx says\n")
	write(t, filepath.Join(f.Paths.MariaDBLogDir, "error.log"), "maria says\n")
	write(t, filepath.Join(f.Paths.LogRoot, "docs.example.com", "php-error.log"), "php says\n")
	ctx := context.Background()
	for _, c := range []struct {
		p    rpc.TailLogParams
		path string
		body string
	}{
		{rpc.TailLogParams{Source: "nginx-error"}, filepath.Join(f.Paths.NginxLogDir, "error.log"), "nginx says"},
		{rpc.TailLogParams{Source: "nginx-access"}, filepath.Join(f.Paths.NginxLogDir, "access.log"), noLogYet},
		{rpc.TailLogParams{Source: "mariadb"}, filepath.Join(f.Paths.MariaDBLogDir, "error.log"), "maria says"},
		{rpc.TailLogParams{Source: "site:docs.example.com:php", Site: "docs.example.com"}, filepath.Join(f.Paths.LogRoot, "docs.example.com", "php-error.log"), "php says"},
		{rpc.TailLogParams{Source: "site:docs.example.com:php"}, filepath.Join(f.Paths.LogRoot, "docs.example.com", "php-error.log"), "php says"},
		{rpc.TailLogParams{Source: "site:docs.example.com:access", Site: "docs.example.com"}, filepath.Join(f.Paths.LogRoot, "docs.example.com", "access.log"), noLogYet},
	} {
		res, err := f.TailLog(ctx, c.p)
		if err != nil || res.Path != c.path || res.Content != c.body {
			t.Errorf("%+v = %+v, %v", c.p, res, err)
		}
	}
	for _, p := range []rpc.TailLogParams{
		{Source: "syslog"},
		{Source: "/etc/shadow"},
		{Source: "site:docs.example.com:shadow", Site: "docs.example.com"},
		{Source: "site:../../etc:access", Site: "../../etc"},
		{Source: "site:docs.example.com:access", Site: "other.example.com"},
		{Source: "site:", Site: "docs.example.com"},
	} {
		if _, err := f.TailLog(ctx, p); codeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v: err = %v, want invalid", p, err)
		}
	}
	if len(f.exec.Calls()) != 0 {
		t.Fatalf("file sources ran %v", f.exec.Calls())
	}
}

func journalLine(us int64, ident, pid string, msg string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d","_HOSTNAME":"box","SYSLOG_IDENTIFIER":%q,"_PID":%q,"MESSAGE":%s}`, us, ident, pid, msg)
}

// Journal sources are read as typed JSON, one entry per line.
func TestTailLogJournal(t *testing.T) {
	f := newFixture(t)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	out := strings.Join([]string{
		journalLine(at.UnixMicro(), "panel-agent", "41", `"started"`),
		journalLine(at.Add(time.Second).UnixMicro(), "panel-agent", "41", `[104,105,255]`), // not UTF-8
		journalLine(at.Add(2*time.Second).UnixMicro(), "panel-agent", "41", `null`),
		`{"__REALTIME_TIMESTAMP":"1","_COMM":"php-fpm8.4","SYSLOG_PID":"7","MESSAGE":["first","second"]}`,
	}, "\n") + "\n"
	f.respond(map[string]host.FakeReply{"journalctl": {Stdout: out}})
	res, err := f.TailLog(context.Background(), rpc.TailLogParams{Source: "agent", Lines: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "journal:cloudground-agent" {
		t.Fatalf("path = %s", res.Path)
	}
	lines := strings.Split(res.Content, "\n")
	stamp := at.Local().Format(time.RFC3339)
	if len(lines) != 4 || lines[0] != stamp+" box panel-agent[41]: started" ||
		!strings.HasSuffix(lines[1], ": hi�") || !strings.HasSuffix(lines[2], "panel-agent[41]: ") ||
		!strings.HasSuffix(lines[3], " php-fpm8.4[7]: first") {
		t.Fatalf("content:\n%s", res.Content)
	}
	if c := f.exec.Calls(); len(c) != 1 || c[0].Line() != "journalctl --no-pager -q -o json -n 200 -u cloudground-agent.service" {
		t.Fatalf("calls = %v", c)
	}
}

// php-error reads every site master; a journalctl failure is an error, not
// an empty log (the old php-error path swallowed it).
func TestTailLogJournalErrors(t *testing.T) {
	f := newFixture(t)
	f.respond(map[string]host.FakeReply{"journalctl": {Code: 1, Stderr: "Failed to open journal"}})
	if _, err := f.TailLog(context.Background(), rpc.TailLogParams{Source: "php-error", Lines: 50}); err == nil {
		t.Fatal("a failed journalctl read as an empty log")
	}
	if c := f.exec.Calls(); len(c) != 1 || c[0].Line() != "journalctl --no-pager -q -o json -n 50 -u cloudground-php-*.service" {
		t.Fatalf("calls = %v", c)
	}
	f.respond(map[string]host.FakeReply{"journalctl": {Stdout: "{not json\n"}})
	if _, err := f.TailLog(context.Background(), rpc.TailLogParams{Source: "api"}); err == nil {
		t.Fatal("undecodable journal output was accepted")
	}
	f.respond(nil)
	res, err := f.TailLog(context.Background(), rpc.TailLogParams{Source: "api"})
	if err != nil || res.Content != noEntries || res.Path != "journal:cloudground-api" {
		t.Fatalf("empty journal = %+v %v", res, err)
	}
}
