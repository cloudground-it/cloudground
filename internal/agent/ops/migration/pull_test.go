package migration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// oldHost is an SSH server with SFTP on 127.0.0.1, password "pw" for
// "old", serving the real filesystem, as an old host would.
func oldHost(t *testing.T) (addr string, fingerprint string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "old" && string(pw) == "pw" {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTP(conn, cfg)
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func serveSFTP(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, in, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					if srv, err := sftp.NewServer(ch); err == nil {
						srv.Serve()
					}
					ch.Close()
					return
				}
				if req.Type == "exec" {
					req.Reply(false, nil)
				}
			}
		}()
	}
}

func pullOps(t *testing.T) (*Ops, string, state.Site) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "site")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	site := state.Site{ID: 3, Domain: "new.example.com", RootPath: root}
	o := &Ops{
		Open: func(state.Site) (*sitefs.Root, error) {
			return sitefs.Open(root, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
		},
		FreeSpace: func(string) (int64, error) { return 100 << 30, nil },
	}
	return o, root, site
}

func oldSite(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"wp-config.php":                        "<?php define('DB_NAME', 'wp'); define( 'DB_USER', \"u\" ); define('DB_PASSWORD', 'p\\'w'); define('DB_HOST', 'localhost');",
		"index.php":                            "<?php // index",
		"wp-content/uploads/2026/10/photo.jpg": "jpeg",
		"db.sql":                               "CREATE TABLE t (id int);",
	} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	os.Symlink("index.php", filepath.Join(dir, "link.php"))
	return dir
}

func TestPullEstimateAndMigration(t *testing.T) {
	addr, fp := oldHost(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	src := oldSite(t)
	o, root, site := pullOps(t)
	base := rpc.PullParams{Site: site, Host: host, Port: port, User: "old", Password: "pw", Path: src}

	est, err := o.PullEstimate(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if est.Files != 4 || est.Bytes == 0 || !est.WordPress || est.HostKey != fp || !est.Fits {
		t.Fatalf("estimate %+v (host key %s)", est, fp)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatal("the dry run wrote something")
	}

	// A wrong password and a pinned key that is not the host's are refused.
	bad := base
	bad.Password = "nope"
	if _, err := o.PullEstimate(context.Background(), bad); err == nil {
		t.Fatal("a wrong password signed in")
	}
	pinned := base
	pinned.HostKey = "SHA256:not-the-key"
	if _, err := o.PullEstimate(context.Background(), pinned); err == nil || !strings.Contains(err.Error(), "not the one given") {
		t.Fatalf("a wrong pin: %v", err)
	}

	pull := base
	pull.HostKey, pull.ReleaseID, pull.DB, pull.SQLPath = fp, "20261027120000", "file", filepath.Join(src, "db.sql")
	res, err := o.PullMigration(context.Background(), pull)
	if err != nil {
		t.Fatal(err)
	}
	if res.Archive != "tmp/cg-pull-20261027120000.tar.gz" || res.SQL != "tmp/cg-pull-20261027120000.sql" || res.Files != 4 {
		t.Fatalf("result %+v", res)
	}
	sql, _ := os.ReadFile(filepath.Join(root, res.SQL))
	if string(sql) != "CREATE TABLE t (id int);" {
		t.Fatalf("dump %q", sql)
	}
	f, _ := os.Open(filepath.Join(root, res.Archive))
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b) + h.Linkname
	}
	f.Close()
	if got["wp-content/uploads/2026/10/photo.jpg"] != "jpeg" || got["link.php"] != "index.php" || got["index.php"] != "<?php // index" {
		t.Fatalf("archive %v", got)
	}
	// A pull for the same release does not overwrite; cleanup removes both.
	if _, err := o.PullMigration(context.Background(), pull); err == nil {
		t.Fatal("a second pull overwrote the first one's files")
	}
	if out, err := o.PullCleanup(context.Background(), rpc.PullCleanupParams{Site: site, ReleaseID: "20261027120000"}); err != nil || out["removed"] != 2 {
		t.Fatalf("cleanup %v %v", out, err)
	}
	// db: dump needs a shell, which this host does not give.
	dump := base
	dump.ReleaseID, dump.DB = "20261027120001", "dump"
	if _, err := o.PullMigration(context.Background(), dump); err == nil {
		t.Fatal("a dump without a shell succeeded")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "tmp")); len(entries) != 0 {
		t.Fatalf("a failed pull left %v", entries)
	}
}

func TestWPDatabaseAndDumpCommand(t *testing.T) {
	name, user, pw, host, err := WPDatabase("define('DB_NAME', 'wp_db');\ndefine(\"DB_USER\", \"admin\");\ndefine('DB_PASSWORD', 'it\\'s');\ndefine('DB_HOST', 'db.internal:3307');")
	if err != nil || name != "wp_db" || user != "admin" || pw != "it's" || host != "db.internal:3307" {
		t.Fatalf("%q %q %q %q %v", name, user, pw, host, err)
	}
	if _, _, _, _, err := WPDatabase("<?php // nothing"); err == nil {
		t.Fatal("a config without a database passed")
	}
	if got := dumpCommand("wp'db", "db.internal:3307"); !strings.Contains(got, `-h 'db.internal' -P '3307' 'wp'\''db'`) || !strings.Contains(got, "--defaults-extra-file=/dev/stdin") {
		t.Fatalf("command %s", got)
	}
	if got := dumpCommand("wp", "localhost:/run/mysqld/mysqld.sock"); !strings.Contains(got, `-S '/run/mysqld/mysqld.sock'`) {
		t.Fatalf("socket command %s", got)
	}
	if optionValue(`a"b\c`) != `"a\"b\\c"` {
		t.Fatalf("option value %s", optionValue(`a"b\c`))
	}
}
