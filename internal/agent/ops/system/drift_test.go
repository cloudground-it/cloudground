package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	installfiles "github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// The drift check hashes as root, so it touches only files the panel
// manages; anything else is handed back unopened, never failing the check.
func TestDriftRefusesUnmanagedPaths(t *testing.T) {
	f := newFixture(t)
	vhost := filepath.Join(f.Paths.NginxSites, "docs.example.com.conf")
	unit := filepath.Join(f.Paths.SystemdDir, "cloudground-php-cg-site-9.service")
	conf := filepath.Join(f.Paths.PHPMasterDir, "cg-site-9.conf")
	tuning := filepath.Join(f.Paths.MariaDBConf, "99-cloudground.cnf")
	conn := filepath.Join(f.Paths.SitesRoot, "docs.example.com", "current", "wp-content", "mu-plugins", "cloudground-connector.php")
	auth := filepath.Join(f.Paths.NginxAuthDir, "staging.example.com.htpasswd") // never written: deleted
	dropin := filepath.Join(f.Paths.SystemdDir, "cloudground-php-cg-site-9.service.d", "cloudground-slice.conf")
	for _, p := range []string{vhost, unit, conf, tuning, conn, dropin} {
		write(t, p, "managed")
	}
	shadow := filepath.Join(f.dir, "shadow")
	write(t, shadow, "secret")
	otherUnit := filepath.Join(f.Paths.SystemdDir, "ssh.service")
	write(t, otherUnit, "x")
	notManaged := []string{
		shadow, otherUnit, "/etc/shadow", "relative/path",
		f.Paths.NginxSites + "/../shadow",
		filepath.Join(f.Paths.SitesRoot, "docs.example.com", "wp-config.php"),
		filepath.Join(f.Paths.SystemdDir, "sub", "cloudground-php-x.service"),
		filepath.Join(f.Paths.SystemdDir, "ssh.service.d", "override.conf"),
		filepath.Join(f.Paths.SystemdDir, "cloudground-php-x.service.d", "sub", "x.conf"),
	}
	expected := map[string]string{vhost: sum("managed"), unit: sum("managed"), conf: "stale", tuning: sum("managed"), conn: sum("managed"), auth: sum("x"), dropin: sum("managed")}
	for _, p := range notManaged {
		expected[p] = "x"
	}
	res, err := f.CheckDrift(context.Background(), rpc.DriftParams{Expected: expected})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(res.NotManaged)
	sort.Strings(notManaged)
	if len(res.NotManaged) != len(notManaged) {
		t.Fatalf("not managed = %v, want %v", res.NotManaged, notManaged)
	}
	for i := range notManaged {
		if res.NotManaged[i] != notManaged[i] {
			t.Fatalf("not managed = %v, want %v", res.NotManaged, notManaged)
		}
	}
	got := map[string]string{}
	for _, d := range res.Drifted {
		got[d.Path] = d.ActualHash
	}
	if len(got) != 2 || got[conf] != sum("managed") || got[auth] != "" {
		t.Fatalf("drifted = %+v", res.Drifted)
	}
}

// A file the site can replace (the connector) must not stall or fail the
// check: a FIFO at its name hung the old check for good (a blocking open),
// and a symlink failed it for every site (ELOOP). Both are drift.
func TestDriftSurvivesWhatASiteCanPlant(t *testing.T) {
	f := newFixture(t)
	mu := filepath.Join(f.Paths.SitesRoot, "docs.example.com", "current", "wp-content", "mu-plugins")
	fifo := filepath.Join(mu, "cloudground-connector.php")
	link := filepath.Join(mu, "object-cache.php")
	dir := filepath.Join(mu, "cloudground-dir.php")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(f.dir, "secret"), "secret")
	if err := os.Symlink(filepath.Join(f.dir, "secret"), link); err != nil {
		t.Fatal(err)
	}
	done := make(chan rpc.DriftResult, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := f.CheckDrift(context.Background(), rpc.DriftParams{Expected: map[string]string{fifo: "a", link: sum("secret"), dir: "c"}})
		errc <- err
		done <- res
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("the check failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drift check blocked on a FIFO")
	}
	res := <-done
	if len(res.Drifted) != 3 {
		t.Fatalf("drifted = %+v", res.Drifted)
	}
	for _, d := range res.Drifted {
		if d.ActualHash != "" {
			t.Fatalf("%s hashed as %s: the planted file was read", d.Path, d.ActualHash)
		}
	}
}

// The connector's directories belong to the site user: a mu-plugins/ turned
// into a symlink to a directory elsewhere must not have root hash the file
// of the same name there.
func TestDriftDoesNotFollowASymlinkedPluginDirectory(t *testing.T) {
	f := newFixture(t)
	wpc := filepath.Join(f.Paths.SitesRoot, "docs.example.com", "current", "wp-content")
	if err := os.MkdirAll(wpc, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.dir, "elsewhere")
	write(t, filepath.Join(outside, "cloudground-connector.php"), "outside")
	mu := filepath.Join(wpc, "mu-plugins")
	os.RemoveAll(mu)
	if err := os.Symlink(outside, mu); err != nil {
		t.Fatal(err)
	}
	conn := filepath.Join(mu, "cloudground-connector.php")
	res, err := f.CheckDrift(context.Background(), rpc.DriftParams{Expected: map[string]string{conn: sum("outside")}})
	if err != nil {
		t.Fatalf("the check failed: %v", err)
	}
	if len(res.Drifted) != 1 || res.Drifted[0].ActualHash != "" {
		t.Fatalf("root hashed a file outside the site through a symlinked directory: %+v", res.Drifted)
	}
}

// The installer's files are the drift check's too: one list (R-6.1).
func TestDriftAcceptsTheInstallersFiles(t *testing.T) {
	f := newFixture(t)
	expected := map[string]string{}
	for _, m := range installfiles.Files {
		expected[m.Path] = "x"
	}
	res, err := f.CheckDrift(context.Background(), rpc.DriftParams{Expected: expected})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NotManaged) != 0 {
		t.Fatalf("the installer's files refused as not managed: %v", res.NotManaged)
	}
	if len(res.Drifted) != len(installfiles.Files) {
		t.Fatalf("drifted = %d files, want all %d (none has hash x)", len(res.Drifted), len(installfiles.Files))
	}
}
