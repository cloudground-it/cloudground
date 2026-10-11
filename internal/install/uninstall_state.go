package install

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The panel's state, before an uninstall removes it (SPEC §9.3). The backup
// repositories are kept, and without their password a backup cannot be read:
// so the state database and its key are copied, root-only, into the kept
// directory, and the backup settings and credentials are written out in
// plain words beside them.

const (
	stateDB   = "/var/lib/cloudground/state.db"
	secretKey = "/var/lib/cloudground/secret.key"
)

// KeptState is what keepPanelState found.
type KeptState struct {
	Repositories []string // every repository the panel used, current first
	Credentials  bool     // backup.env was written
}

// keepPanelState copies the state into keptDir/state and writes
// keptDir/backup.env and keptDir/README.txt. The copy is made before any
// decryption, so a key or schema this build cannot read still leaves it.
func (e *Env) keepPanelState(keptDir string) error {
	if err := e.setAsideEarlier(keptDir); err != nil {
		return err
	}
	kept, err := e.copyState(keptDir)
	if err != nil {
		return err
	}
	ks, rerr := readBackupSettings(kept, keptDir, e)
	if rerr != nil {
		e.logf("warning: the backup credentials could not be read from the copy: %v", rerr)
	}
	return e.writeKeptReadme(keptDir, ks, rerr)
}

// keptNames are what an uninstall leaves in the kept directory, its log
// aside.
var keptNames = []string{"state", "backup.env", KeptReadme, "custom-certs"}

// setAsideEarlier moves what an earlier uninstall kept (a server
// uninstalled, installed again, and uninstalled again) into
// keptDir/earlier-<UTC time>/, so that its state and its backup
// credentials, which may be the only way into an older repository, are not
// overwritten by this run's.
func (e *Env) setAsideEarlier(keptDir string) error {
	var present []string
	for _, n := range keptNames {
		if _, err := os.Lstat(e.path(filepath.Join(keptDir, n))); err == nil {
			present = append(present, n)
		}
	}
	if len(present) == 0 {
		return nil
	}
	if err := os.Chmod(e.path(keptDir), 0o700); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(keptDir, "earlier-"+stamp)
	for i := 2; ; i++ {
		err := os.Mkdir(e.path(dir), 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		dir = filepath.Join(keptDir, fmt.Sprintf("earlier-%s-%d", stamp, i))
	}
	for _, n := range present {
		if err := os.Rename(e.path(filepath.Join(keptDir, n)), e.path(filepath.Join(dir, n))); err != nil {
			return fmt.Errorf("set aside the earlier uninstall's %s: %w", n, err)
		}
	}
	e.logf("what an earlier uninstall kept was moved to %s", dir)
	return nil
}

// copyState copies the database, its WAL and the key into keptDir/state
// (0700, files 0600) and answers the directory.
func (e *Env) copyState(keptDir string) (string, error) {
	dir := filepath.Join(keptDir, "state")
	if err := os.MkdirAll(e.path(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(e.path(keptDir), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(e.path(dir), 0o700); err != nil {
		return "", err
	}
	for _, p := range []string{stateDB, stateDB + "-wal", secretKey} {
		err := copyNoFollow(e.path(p), e.path(filepath.Join(dir, filepath.Base(p))))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("keep %s: %w", p, err)
		}
	}
	return dir, nil
}

// readBackupSettings opens a scratch copy of the kept state (an Open may
// migrate or compact what it opens, and the kept copy stays as it was) and
// writes backup.env from it.
func readBackupSettings(stateDir, keptDir string, e *Env) (KeptState, error) {
	var ks KeptState
	scratch, err := os.MkdirTemp(e.path(keptDir), ".read-")
	if err != nil {
		return ks, err
	}
	defer os.RemoveAll(scratch)
	for _, n := range []string{"state.db", "state.db-wal", "secret.key"} {
		if err := copyNoFollow(filepath.Join(e.path(stateDir), n), filepath.Join(scratch, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ks, err
		}
	}
	if _, err := os.Stat(filepath.Join(scratch, "state.db")); err != nil {
		return ks, nil // no database: nothing was configured
	}
	st, err := state.OpenWith(filepath.Join(scratch, "state.db"), state.Options{SecretKeyPath: filepath.Join(scratch, "secret.key")})
	if err != nil {
		return ks, err
	}
	defer st.Close()
	ps, err := st.PanelSettings()
	if err != nil {
		return ks, err
	}
	repos, err := st.BackupRepositories()
	if err != nil {
		return ks, err
	}
	if ps.BackupRepository != "" {
		ks.Repositories = append(ks.Repositories, ps.BackupRepository)
	}
	for _, r := range repos {
		if !slices.Contains(ks.Repositories, r) {
			ks.Repositories = append(ks.Repositories, r)
		}
	}
	if ps.BackupRepository == "" {
		return ks, nil
	}
	pw, err := st.Secret(state.SecretBackupPassword)
	if err != nil {
		return ks, err
	}
	s3secret, err := st.Secret(state.SecretBackupS3SecretKey)
	if err != nil {
		return ks, err
	}
	var b strings.Builder
	b.WriteString("# CloudGround's backup repository, kept by `cgctl uninstall`.\n")
	b.WriteString("# With restic: set -a; . ./backup.env; set +a; restic snapshots\n")
	fmt.Fprintf(&b, "RESTIC_REPOSITORY=%s\n", shellQuote(ps.BackupRepository))
	fmt.Fprintf(&b, "RESTIC_PASSWORD=%s\n", shellQuote(pw))
	if ps.BackupS3AccessKey != "" {
		fmt.Fprintf(&b, "AWS_ACCESS_KEY_ID=%s\n", shellQuote(ps.BackupS3AccessKey))
		fmt.Fprintf(&b, "AWS_SECRET_ACCESS_KEY=%s\n", shellQuote(s3secret))
	}
	if ps.BackupS3Region != "" {
		fmt.Fprintf(&b, "AWS_DEFAULT_REGION=%s\n", shellQuote(ps.BackupS3Region))
	}
	if err := writePrivate(e.path(filepath.Join(keptDir, "backup.env")), []byte(b.String())); err != nil {
		return ks, err
	}
	ks.Credentials = true
	return ks, nil
}

// KeptReadme is the name of the note left in the kept directory.
const KeptReadme = "README.txt"

func (e *Env) writeKeptReadme(keptDir string, ks KeptState, readErr error) error {
	var b strings.Builder
	b.WriteString("CloudGround was uninstalled from this server. This directory holds what it kept for you.\n\n")
	b.WriteString("state/       the panel's database (state.db) and its key (secret.key), as they were.\n")
	b.WriteString("             They hold every secret the panel kept: keep this directory root-only.\n")
	switch {
	case ks.Credentials:
		b.WriteString("backup.env   the backup repository and its credentials, for restic, in plain text:\n")
		b.WriteString("             store it somewhere safe, off this server, then delete it.\n")
	case readErr != nil:
		fmt.Fprintf(&b, "             The backup credentials could not be read (%v):\n", readErr)
		b.WriteString("             they are in state/, readable by a CloudGround of the same or a newer release.\n")
	}
	if len(ks.Repositories) > 0 {
		b.WriteString("\nThe backup repositories were NOT touched; they still hold every backup:\n")
		for _, r := range ks.Repositories {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}
	return writePrivate(e.path(filepath.Join(keptDir, KeptReadme)), []byte(b.String()))
}

// KeptSummary reads back what an uninstall kept, for the end of the run.
func (e *Env) KeptSummary(keptDir string) (repos []string, credentials bool) {
	b, err := e.readFile(filepath.Join(keptDir, KeptReadme))
	if err == nil {
		inRepos := false
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "The backup repositories") {
				inRepos = true
				continue
			}
			if inRepos && strings.HasPrefix(l, "  ") {
				repos = append(repos, strings.TrimSpace(l))
			}
		}
	}
	return repos, e.exists(filepath.Join(keptDir, "backup.env"))
}

// shellQuote quotes a value for a POSIX shell's `.`.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	return os.Rename(tmp, path)
}

// copyNoFollow copies a regular file, refusing a link at from, into a new
// 0600 file at to (replacing one there).
func copyNoFollow(from, to string) error {
	src, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	_ = os.Remove(to)
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}
