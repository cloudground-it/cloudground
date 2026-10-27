package migration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Pulling a site from another host (SPEC §8, #82): the old site's folder,
// and its database, fetched over SSH/SFTP into the site's tmp/ for the
// import. The credentials live in memory for the call only.

const (
	pullDialTimeout = 30 * time.Second
	pullPrefix      = "tmp/cg-pull-"
	maxWPConfig     = 1 << 20
)

// pullConn is an open connection to the old host.
type pullConn struct {
	ssh     *ssh.Client
	sftp    *sftp.Client
	hostKey string
}

func (c *pullConn) Close() {
	if c.sftp != nil {
		c.sftp.Close()
	}
	if c.ssh != nil {
		c.ssh.Close()
	}
}

// Dial is how the agent reaches the old host; tests replace it.
var pullDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: pullDialTimeout}
	return d.DialContext(ctx, network, addr)
}

// connect opens SSH and SFTP to the old host, checking its key against the
// pin when there is one.
func connect(ctx context.Context, p rpc.PullParams) (*pullConn, error) {
	var auth []ssh.AuthMethod
	if p.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(p.PrivateKey))
		if err != nil {
			return nil, agentrpc.Invalid("the private key cannot be read (an encrypted key is not taken)")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	} else {
		pw := p.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = pw
			}
			return out, nil
		}))
	}
	seen := ""
	port := p.Port
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User: p.User, Auth: auth, Timeout: pullDialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if p.HostKey != "" && seen != p.HostKey {
				return fmt.Errorf("the host's key is %s, not the one given", seen)
			}
			return nil
		},
	}
	addr := net.JoinHostPort(strings.Trim(p.Host, "[]"), strconv.Itoa(port))
	raw, err := pullDial(ctx, "tcp", addr)
	if err != nil {
		return nil, agentrpc.Invalid("cannot reach %s: %v", addr, err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close()
		if seen != "" && p.HostKey != "" && seen != p.HostKey {
			return nil, agentrpc.Invalid("the host's key is %s, not the one given: refusing to connect", seen)
		}
		return nil, agentrpc.Invalid("the sign-in to %s@%s was refused", p.User, addr)
	}
	client := ssh.NewClient(cc, chans, reqs)
	sc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, agentrpc.Invalid("the account has no SFTP: %v", err)
	}
	return &pullConn{ssh: client, sftp: sc, hostKey: seen}, nil
}

// pullEntry is one entry of the old site's folder.
type pullEntry struct {
	rel  string
	info os.FileInfo
	link string
}

// walk lists the old site's folder (its root resolved once), within the
// import's limits.
func walk(ctx context.Context, sc *sftp.Client, root string, lim Limits) (string, []pullEntry, int, int64, error) {
	real, err := sc.RealPath(root)
	if err != nil {
		return "", nil, 0, 0, agentrpc.Invalid("the folder %s cannot be found on the old host", root)
	}
	if st, err := sc.Stat(real); err != nil || !st.IsDir() {
		return "", nil, 0, 0, agentrpc.Invalid("%s is not a folder on the old host", root)
	}
	var out []pullEntry
	files, bytes := 0, int64(0)
	w := sc.Walk(real)
	for w.Step() {
		if ctx.Err() != nil {
			return "", nil, 0, 0, ctx.Err()
		}
		if err := w.Err(); err != nil {
			return "", nil, 0, 0, agentrpc.Invalid("cannot read %s on the old host: %v", w.Path(), err)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(w.Path(), real), "/")
		if rel == "" {
			continue
		}
		fi := w.Stat()
		e := pullEntry{rel: rel, info: fi}
		switch {
		case fi.Mode().IsRegular():
			files++
			bytes += fi.Size()
		case fi.Mode()&os.ModeSymlink != 0:
			if e.link, err = sc.ReadLink(w.Path()); err != nil {
				continue
			}
		case fi.IsDir():
		default:
			continue
		}
		out = append(out, e)
		if len(out) > lim.MaxEntries || files > lim.MaxFiles || bytes > lim.MaxBytes {
			return "", nil, 0, 0, agentrpc.Invalid("the old site is larger than a migration takes")
		}
	}
	return real, out, files, bytes, nil
}

// pullLimits are the import's limits.
func (o *Ops) pullLimits() Limits {
	if o.Limits == (Limits{}) {
		return DefaultLimits
	}
	return o.Limits
}

// free is the space free on the site's filesystem.
func (o *Ops) free(s state.Site) (int64, error) { return o.freeSpace(s.RootPath) }

// fits reports whether the pulled files, their import (about as much
// again) and the reserve fit.
func fits(bytes, free int64, lim Limits) bool { return 2*bytes+lim.Reserve <= free }

// shellOK reports whether the account runs a command.
func shellOK(c *ssh.Client) bool {
	sess, err := c.NewSession()
	if err != nil {
		return false
	}
	defer sess.Close()
	return sess.Run("exit 0") == nil
}

// PullEstimate is the dry run: what a pull would copy, and whether it fits.
func (o *Ops) PullEstimate(ctx context.Context, p rpc.PullParams) (rpc.PullEstimateResult, error) {
	c, err := connect(ctx, p)
	if err != nil {
		return rpc.PullEstimateResult{}, err
	}
	defer c.Close()
	lim := o.pullLimits()
	real, entries, files, bytes, err := walk(ctx, c.sftp, p.Path, lim)
	if err != nil {
		return rpc.PullEstimateResult{}, err
	}
	res := rpc.PullEstimateResult{Files: files, Bytes: bytes, HostKey: c.hostKey}
	for _, e := range entries {
		if e.rel == "wp-config.php" {
			res.WordPress = true
		}
	}
	if !res.WordPress {
		if _, err := c.sftp.Stat(path.Join(real, "wp-config.php")); err == nil {
			res.WordPress = true
		}
	}
	if p.DB == "dump" {
		res.Shell = shellOK(c.ssh)
	}
	if res.FreeBytes, err = o.free(p.Site); err != nil {
		return rpc.PullEstimateResult{}, err
	}
	res.Fits = fits(bytes, res.FreeBytes, lim)
	return res, nil
}

// PullMigration writes the old site's archive (and dump) into the site's
// tmp/ for the import.
func (o *Ops) PullMigration(ctx context.Context, p rpc.PullParams) (res rpc.PullResult, err error) {
	if p.ReleaseID == "" {
		return res, agentrpc.Invalid("release id required")
	}
	c, err := connect(ctx, p)
	if err != nil {
		return res, err
	}
	defer c.Close()
	lim := o.pullLimits()
	real, entries, files, bytes, err := walk(ctx, c.sftp, p.Path, lim)
	if err != nil {
		return res, err
	}
	free, err := o.free(p.Site)
	if err != nil {
		return res, err
	}
	if !fits(bytes, free, lim) {
		return res, agentrpc.Invalid("the old site (%d bytes) does not fit in the space free on this server (%d bytes, with the import's room)", bytes, free)
	}
	r, err := o.Open(p.Site)
	if err != nil {
		return res, err
	}
	defer r.Close()
	if err := r.MkdirAll("tmp", 0o750); err != nil {
		return res, err
	}
	archive := pullPrefix + p.ReleaseID + ".tar.gz"
	sqlRel := pullPrefix + p.ReleaseID + ".sql"
	// Files of an earlier pull of the same release are never replaced.
	for _, rel := range []string{archive, sqlRel} {
		if _, lerr := r.Lstat(rel); lerr == nil {
			return res, agentrpc.Invalid("%s is already there", rel)
		}
	}
	defer func() {
		// A failure removes only what this call wrote.
		if err != nil {
			_ = r.Remove(archive)
			_ = r.Remove(sqlRel)
		}
	}()
	if err := writeArchive(ctx, c.sftp, real, entries, r, archive); err != nil {
		return res, fmt.Errorf("copy the old site's files: %w", err)
	}
	res = rpc.PullResult{Archive: archive, Files: files, Bytes: bytes, HostKey: c.hostKey}
	switch p.DB {
	case "file":
		res.SQL = sqlRel
		if err := copyRemote(ctx, c.sftp, p.SQLPath, r, res.SQL); err != nil {
			return res, fmt.Errorf("copy the database dump: %w", err)
		}
	case "dump":
		res.SQL = sqlRel
		if err := dumpRemote(ctx, c, path.Join(real, "wp-config.php"), r, res.SQL); err != nil {
			return res, err
		}
	}
	return res, nil
}

// create makes a new file in the site's tree, owned by the site user; one
// already there is refused.
func create(r *sitefs.Root, rel string) (*os.File, error) {
	if _, err := r.Lstat(rel); err == nil {
		return nil, fmt.Errorf("%s already exists", rel)
	}
	return r.Create(rel, 0o640)
}

// writeArchive streams the old site's entries into a gzip'd tar in the
// site's tree.
func writeArchive(ctx context.Context, sc *sftp.Client, real string, entries []pullEntry, r *sitefs.Root, rel string) error {
	f, err := create(r, rel)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	werr := func() error {
		for _, e := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h := &tar.Header{Name: e.rel, ModTime: e.info.ModTime(), Mode: int64(e.info.Mode().Perm())}
			switch {
			case e.info.IsDir():
				h.Typeflag, h.Name = tar.TypeDir, e.rel+"/"
			case e.info.Mode()&os.ModeSymlink != 0:
				h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
			default:
				h.Typeflag, h.Size = tar.TypeReg, e.info.Size()
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			src, err := sc.Open(path.Join(real, e.rel))
			if err != nil {
				return fmt.Errorf("open %s: %w", e.rel, err)
			}
			n, err := io.Copy(tw, io.LimitReader(src, h.Size))
			src.Close()
			if err != nil {
				return fmt.Errorf("read %s: %w", e.rel, err)
			}
			if n != h.Size {
				return fmt.Errorf("%s changed while it was copied", e.rel)
			}
		}
		return nil
	}()
	err = errors.Join(werr, tw.Close(), gz.Close(), f.Sync(), f.Close())
	return err
}

// copyRemote copies a file of the old host into the site's tree.
func copyRemote(ctx context.Context, sc *sftp.Client, remote string, r *sitefs.Root, rel string) error {
	src, err := sc.Open(remote)
	if err != nil {
		return agentrpc.Invalid("%s cannot be read on the old host", remote)
	}
	defer src.Close()
	f, err := create(r, rel)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, ctxReader{ctx, src})
	return errors.Join(err, f.Sync(), f.Close())
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// wpDefineRe reads a define('NAME', 'value') line of wp-config.php.
var wpDefineRe = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"](DB_NAME|DB_USER|DB_PASSWORD|DB_HOST)['"]\s*,\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")\s*\)`)

// WPDatabase reads the database settings of a wp-config.php.
func WPDatabase(config string) (name, user, password, host string, err error) {
	vals := map[string]string{}
	for _, m := range wpDefineRe.FindAllStringSubmatch(config, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		vals[m[1]] = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v)
	}
	name, user, password, host = vals["DB_NAME"], vals["DB_USER"], vals["DB_PASSWORD"], vals["DB_HOST"]
	if name == "" || user == "" {
		return "", "", "", "", agentrpc.Invalid("the old site's wp-config.php names no database (DB_NAME, DB_USER)")
	}
	if host == "" {
		host = "localhost"
	}
	return name, user, password, host, nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// dumpCommand is the mysqldump run on the old host; its credentials go to
// its standard input.
func dumpCommand(name, host string) string {
	args := []string{"mysqldump", "--defaults-extra-file=/dev/stdin", "--single-transaction", "--quick", "--no-tablespaces", "--default-character-set=utf8mb4"}
	h, port, hasPort := strings.Cut(host, ":")
	switch {
	case hasPort && strings.HasPrefix(port, "/"):
		args = append(args, "-S", shellQuote(port))
	case hasPort:
		args = append(args, "-h", shellQuote(h), "-P", shellQuote(port))
	default:
		args = append(args, "-h", shellQuote(h))
	}
	return strings.Join(append(args, shellQuote(name)), " ")
}

// dumpRemote runs mysqldump on the old host with the database settings of
// its wp-config.php, its output streamed into the site's tree.
func dumpRemote(ctx context.Context, c *pullConn, config string, r *sitefs.Root, rel string) error {
	cf, err := c.sftp.Open(config)
	if err != nil {
		return agentrpc.Invalid("the old site has no wp-config.php to read its database from")
	}
	b, err := io.ReadAll(io.LimitReader(cf, maxWPConfig))
	cf.Close()
	if err != nil {
		return err
	}
	name, user, password, host, err := WPDatabase(string(b))
	if err != nil {
		return err
	}
	sess, err := c.ssh.NewSession()
	if err != nil {
		return agentrpc.Invalid("the account has no shell to run mysqldump: give the dump's path instead")
	}
	defer sess.Close()
	f, err := create(r, rel)
	if err != nil {
		return err
	}
	var stderr strings.Builder
	sess.Stdin = strings.NewReader("[client]\nuser=" + optionValue(user) + "\npassword=" + optionValue(password) + "\n")
	sess.Stdout = f
	sess.Stderr = &limitedWriter{b: &stderr, n: 4096}
	done := make(chan error, 1)
	go func() { done <- sess.Run(dumpCommand(name, host)) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Close()
		err = ctx.Err()
	}
	cerr := errors.Join(f.Sync(), f.Close())
	if err != nil {
		return agentrpc.Invalid("mysqldump failed on the old host: %s", strings.TrimSpace(stderr.String()))
	}
	return cerr
}

// optionValue quotes a value for a MySQL option file.
func optionValue(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}

type limitedWriter struct {
	b *strings.Builder
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

// PullCleanup removes what a pull wrote for a release.
func (o *Ops) PullCleanup(_ context.Context, p rpc.PullCleanupParams) (map[string]int, error) {
	r, err := o.Open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	n := 0
	for _, ext := range []string{".tar.gz", ".sql"} {
		if err := r.Remove(pullPrefix + p.ReleaseID + ext); err == nil {
			n++
		}
	}
	return map[string]int{"removed": n}, nil
}
