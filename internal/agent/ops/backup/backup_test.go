package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

var ctx = context.Background()

// siteUID owns the site's files when the tests run as root (the container
// suite), so ownership is asserted against a user that is not root.
const siteUID = 4242

const (
	filesID = "f1f1f1f1f1f1f1f1"
	dbID    = "dbdbdbdbdbdbdbdb"
	repoURL = "/var/backups/cloudground/repo"
)

// fakeRestic is a stand-in for restic, first on PATH, driven through the
// real adapter. In its state directory it logs each call's argv to log and
// its repository to repos; exit-<cmd> (cmd "backup-stdin", "backup-files"
// or restic's command) makes it exit with that code (after the summary,
// for a backup); hang-<cmd> makes it start a child and wait, writing both
// pids. It keeps what a stdin backup was fed in stdin, answers
// `snapshots <id>` from snap-<id>.json and a bare `snapshots` from
// all.json, `restore <id>` from the tree tree-<id>, `dump <id>` from
// dump-<id>, and a dry-run forget from plan.
const fakeResticScript = `#!/bin/sh
d="$FAKE_RESTIC"
echo "$*" >> "$d/log"
echo "$RESTIC_REPOSITORY" >> "$d/repos"
cmd=$1; shift
sub=$cmd
if [ "$cmd" = backup ]; then
  case " $* " in *" --stdin "*) sub=backup-stdin ;; *) sub=backup-files ;; esac
fi
if [ -f "$d/hang-$sub" ]; then
  echo $$ > "$d/pid"
  sleep 300 &
  echo $! > "$d/child"
  wait
fi
if [ -f "$d/locking-$sub" ]; then
  # As restic: a lock in the repository while it runs, removed on SIGINT.
  touch "$d/lock"
  trap 'rm -f "$d/lock"; exit 130' INT
  echo $$ > "$d/pid"
  while :; do sleep 0.05; done
fi
if [ -f "$d/exit-$sub" ] && [ "$cmd" != backup ]; then
  echo "restic $sub failed for $RESTIC_REPOSITORY" >&2
  exit $(cat "$d/exit-$sub")
fi
case "$cmd" in
cat|init|check|prune) ;;
forget) case " $* " in *" --dry-run "*) cat "$d/plan" ;; esac ;;
backup)
  if [ $sub = backup-stdin ]; then cat > "$d/stdin"; id=` + dbID + `
  else echo "$*" > "$d/files-args"; id=` + filesID + `; fi
  echo '{"message_type":"status","percent_done":1}'
  echo "{\"message_type\":\"summary\",\"snapshot_id\":\"$id\",\"total_bytes_processed\":7}"
  if [ -f "$d/exit-$sub" ]; then exit $(cat "$d/exit-$sub"); fi
  ;;
snapshots)
  id=
  for a in "$@"; do case "$a" in -*) ;; *) id=$a ;; esac; done
  if [ -n "$id" ]; then cat "$d/snap-$id.json" 2>/dev/null || echo '[]'
  else cat "$d/all.json" 2>/dev/null || echo '[]'; fi ;;
restore) mkdir -p "$3" && cp -a "$d/tree-$1/." "$3/" ;;
dump) cat "$d/dump-$1" ;;
*) echo "unexpected restic $cmd" >&2; exit 1 ;;
esac
`

type fixture struct {
	t     *testing.T
	o     *Ops
	dir   string // the stand-in's state
	site  state.Site
	m     *host.FakeMariaDB
	owner sitefs.Owner
	cache string // the site's page cache

	restarts, refreshes, reloads, purges int
	restartErr                           error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), m: host.NewFakeMariaDB()}
	// site_3 is site 3's, as the agent's record rebuilds it (ops/db SPEC §2).
	f.m.Rows[db.ScanDatabasesQuery] = []string{"site_3"}
	f.m.Rows[db.ScanUsersQuery] = []string{"site_3"}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "restic"), []byte(fakeResticScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RESTIC", f.dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	f.owner = sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	if os.Getuid() == 0 {
		f.owner = sitefs.Owner{UID: siteUID, GID: siteUID}
	}
	sites := t.TempDir()
	f.site = state.Site{ID: 3, Domain: "docs.example.com", Type: state.SiteStatic, SystemUser: "cg-site-3", PHPVersion: "8.4",
		RootPath: filepath.Join(sites, "docs.example.com")}
	f.site.Config.Database.Enabled, f.site.Config.Database.Name = true, "site_3"
	mk(t, f.site.RootPath, map[string]string{
		"releases/initial/index.html": "live",
		"shared/wp-config.php":        "<?php",
		"logs/access.log":             "",
	})
	if err := os.Symlink("releases/initial", filepath.Join(f.site.RootPath, "current")); err != nil {
		t.Fatal(err)
	}
	f.cache = filepath.Join(t.TempDir(), "docs.example.com")
	mk(t, f.cache, map[string]string{"a/bc/0123abc": "stale page"})

	work := t.TempDir()
	f.o = &Ops{
		Restic: host.NewRestic(host.OSExec{}),
		DB:     &db.Ops{MariaDB: f.m, WorkDir: work, RegistryFile: filepath.Join(t.TempDir(), "databases.json")},
		Owner:  func(string) (sitefs.Owner, error) { return f.owner, nil },
		RestartPHP: func(context.Context, state.Site) error {
			f.restarts++
			return f.restartErr
		},
		RefreshPHP: func(context.Context, state.Site) error {
			f.refreshes++
			return nil
		},
		ReloadNginx: func() error {
			f.reloads++
			return nil
		},
		PurgeCache: func(state.Site) error {
			f.purges++
			ents, _ := os.ReadDir(f.cache)
			for _, e := range ents {
				os.RemoveAll(filepath.Join(f.cache, e.Name()))
			}
			return nil
		},
		WorkDir:  work,
		Hostname: func() (string, error) { return "box", nil },
	}
	return f
}

func mk(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) log() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "log"))
	return string(b)
}

// seed lays out a backup of the site as it is now: a files snapshot (its
// tree under the root's absolute path, as restic restores it) tagged with
// its database snapshot when withDB, and that snapshot holding dump.
func (f *fixture) seed(withDB bool, dump string) {
	f.t.Helper()
	dst := filepath.Join(f.dir, "tree-"+filesID, f.site.RootPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", f.site.RootPath, dst).CombinedOutput(); err != nil {
		f.t.Fatalf("copy the site: %v: %s", err, out)
	}
	tags := `"site:docs.example.com","kind:full"`
	if withDB {
		tags += `,"db:` + dbID + `"`
		f.write("snap-"+dbID+".json", `[{"id":"`+dbID+`","hostname":"box","tags":["site:docs.example.com","kind:full","part:database"]}]`)
		f.write("dump-"+dbID, dump)
	}
	f.write("snap-"+filesID+".json", `[{"id":"`+filesID+`","hostname":"box","tags":[`+tags+`]}]`)
}

func (f *fixture) backup(kind string) (rpc.BackupResult, error) {
	return f.o.RunBackup(ctx, rpc.BackupParams{Site: f.site, Repository: repoURL, Password: "pw", Kind: kind})
}

func (f *fixture) restore(mode string) error {
	_, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Mode: mode})
	return err
}

func (f *fixture) verify() rpc.VerifyResult {
	f.t.Helper()
	res, err := f.o.VerifyBackup(ctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID})
	if err != nil {
		f.t.Fatalf("verify: %v", err)
	}
	return res
}

func (f *fixture) live(rel string) string {
	b, _ := os.ReadFile(filepath.Join(f.site.RootPath, rel))
	return string(b)
}

// ------------------------------------------------------------- RunBackup --

// The dump goes into restic on stdin, as a snapshot of its own the files
// snapshot names; nothing is written into the site's tree (N-2). The
// repository and password travel in the environment.
func TestBackupStreamsDatabaseIntoItsOwnSnapshot(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- MariaDB dump 10.19\nCREATE TABLE t (id int);\n"
	res, err := f.backup("full")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if res.SnapshotID != filesID || res.SizeBytes != 14 {
		t.Fatalf("result = %+v, want the files snapshot and both sizes", res)
	}
	if got, _ := os.ReadFile(filepath.Join(f.dir, "stdin")); !strings.Contains(string(got), "CREATE TABLE t") {
		t.Fatalf("restic was not fed the dump: %q", got)
	}
	log := f.log()
	if !strings.Contains(log, "backup --stdin --stdin-filename database.sql --tag site:docs.example.com --tag kind:full --tag part:database --tag dbname:site_3 --tag tables:0 --json --quiet") {
		t.Fatalf("no tagged stdin backup of the database:\n%s", log)
	}
	args, _ := os.ReadFile(filepath.Join(f.dir, "files-args"))
	if !strings.Contains(string(args), "--tag db:"+dbID) || !strings.HasSuffix(strings.TrimSpace(string(args)), f.site.RootPath) {
		t.Fatalf("files snapshot args = %q", args)
	}
	if strings.Contains(log, repoURL) || strings.Contains(log, "pw") {
		t.Fatalf("repository or password in argv:\n%s", log)
	}
	if repos, _ := os.ReadFile(filepath.Join(f.dir, "repos")); !strings.Contains(string(repos), repoURL) {
		t.Fatal("the repository did not reach restic's environment")
	}
	ents, _ := os.ReadDir(filepath.Join(f.site.RootPath, "logs"))
	if len(ents) != 1 {
		t.Fatalf("the backup wrote into the site's logs/: %v", ents)
	}
	if strings.Contains(log, "init") {
		t.Fatal("an initialised repository was initialised again")
	}
}

// The site's tmp/ (PHP's uploads in flight and its sessions, which PHP
// deletes at any moment) stays out of the files snapshot; logs/ and the rest
// of the root are in it.
func TestBackupLeavesTransientDirsOut(t *testing.T) {
	f := newFixture(t)
	if _, err := f.backup("files"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(f.dir, "files-args"))
	got := strings.Fields(string(args))
	want := []string{"--exclude", filepath.Join(f.site.RootPath, "tmp"), f.site.RootPath}
	if len(got) < 3 || strings.Join(got[len(got)-3:], " ") != strings.Join(want, " ") {
		t.Fatalf("files snapshot args = %q, want them to end %q", args, want)
	}
	if strings.Count(string(args), "--exclude") != 1 {
		t.Fatalf("more than tmp/ left out: %q", args)
	}
}

// A restore makes PHP's sessions directory again, which the snapshot does
// not hold: the site user's, 0750, as a new site has it.
func TestRestoreRecreatesSessionsDir(t *testing.T) {
	f := newFixture(t)
	f.seed(false, "")
	if err := f.restore("files"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(f.site.RootPath, "tmp/sessions"))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("tmp/sessions after a restore: %v %v", fi, err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != f.owner.UID || int(st.Gid) != f.owner.GID) {
		t.Fatalf("tmp/sessions belongs to %d:%d, want the site user's %d:%d", st.Uid, st.Gid, f.owner.UID, f.owner.GID)
	}
}

// Files only: no dump, no db: tag.
func TestBackupFilesKindTakesNoDump(t *testing.T) {
	f := newFixture(t)
	if _, err := f.backup("files"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.log(), "--stdin") || len(f.m.Imported) != 0 {
		t.Fatalf("a files backup dumped the database:\n%s", f.log())
	}
	if _, err := f.backup("everything"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("unknown kind: %v", err)
	}
}

// A dump that dies half-way fails the backup and leaves no truncated
// "database" snapshot for a restore to find.
func TestBackupFailedDumpForgetsTruncatedSnapshot(t *testing.T) {
	f := newFixture(t)
	f.m.OnDump = func(_ string, _ []string, w io.Writer) error {
		io.WriteString(w, "-- MariaDB dump 10.19\nCREATE TABLE t (id int);\n")
		return errors.New("mariadb-dump: exit status 2: Lost connection")
	}
	if _, err := f.backup("full"); err == nil || !strings.Contains(err.Error(), "Lost connection") {
		t.Fatalf("a failed dump did not fail the backup: %v", err)
	}
	if !strings.Contains(f.log(), "forget "+dbID) {
		t.Fatalf("the truncated database snapshot was kept:\n%s", f.log())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "files-args")); err == nil {
		t.Fatal("a files snapshot was taken after the dump failed")
	}
}

// A files snapshot that fails (here restic's exit 3: some files could not
// be read, though it made a snapshot) fails the backup and leaves no
// orphan: neither the database snapshot nothing names, nor the partial
// files snapshot.
func TestBackupFailedFilesSnapshotLeavesNoOrphans(t *testing.T) {
	f := newFixture(t)
	f.m.Dumps["site_3"] = "-- MariaDB dump\n"
	f.write("exit-backup-files", "3")
	if _, err := f.backup("full"); err == nil {
		t.Fatal("an incomplete files snapshot passed as a backup")
	}
	log := f.log()
	if !strings.Contains(log, "forget "+dbID) || !strings.Contains(log, "forget "+filesID) {
		t.Fatalf("the snapshots of a failed backup were kept:\n%s", log)
	}
}

// The repository is initialised when it cannot be read, never over a wrong
// password; restic's typed failures reach the user as plain messages,
// without restic's output.
func TestEnsureRepository(t *testing.T) {
	f := newFixture(t)
	f.write("exit-cat", "10")
	if _, err := f.backup("files"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.log(), "\ninit\n") {
		t.Fatalf("a missing repository was not initialised:\n%s", f.log())
	}

	f = newFixture(t)
	f.write("exit-cat", "12")
	_, err := f.o.TestBackup(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"})
	if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || agentrpc.Message(err, "") != "the backup repository password is wrong" {
		t.Fatalf("wrong password = %v (%s)", err, agentrpc.CodeOf(err))
	}
	if !errors.Is(err, host.ErrWrongPassword) {
		t.Fatal("the typed restic error was lost")
	}
	if strings.Contains(f.log(), "init") {
		t.Fatal("a repository with a wrong password was initialised over")
	}
	if e := agentrpc.AsError(err); e == nil || strings.Contains(e.Message, repoURL) {
		t.Fatalf("message = %+v", e)
	}

	// restic before 0.17 exits 1 for everything: init is tried, and its
	// failure does not hide why the repository could not be read.
	f = newFixture(t)
	f.write("exit-cat", "1")
	f.write("exit-init", "1")
	if _, err := f.o.TestBackup(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"}); err == nil ||
		!strings.Contains(err.Error(), "restic cat failed") || !strings.Contains(err.Error(), "initialise") {
		t.Fatalf("unreadable repository = %v", err)
	}
}

func TestTestBackupCountsSnapshots(t *testing.T) {
	f := newFixture(t)
	f.write("all.json", `[{"id":"`+filesID+`"},{"id":"`+dbID+`"}]`)
	res, err := f.o.TestBackup(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"})
	if err != nil || res["snapshots"] != 2 || res["reachable"] != true {
		t.Fatalf("TestBackup = %v %v", res, err)
	}
	f.write("all.json", "not json")
	if _, err := f.o.TestBackup(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"}); err == nil {
		t.Fatal("an unreadable snapshot list passed as reachable")
	}
	if _, err := f.o.TestBackup(ctx, rpc.TestBackupParams{Repository: repoURL}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("no password = %v", err)
	}
}

// Cancelling the request kills restic and everything it started, promptly.
func TestCancelKillsRestic(t *testing.T) {
	f := newFixture(t)
	f.write("hang-backup-files", "")
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := f.o.RunBackup(cctx, rpc.BackupParams{Site: f.site, Repository: repoURL, Password: "pw", Kind: "files"})
		done <- err
	}()
	pids := waitPids(t, f.dir, "pid", "child")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled backup = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunBackup did not return within 5 s of the cancellation")
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running after the cancellation", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// A backup cancelled or timed out mid-run stops restic with SIGINT, so it
// removes its lock from the repository (killed outright it leaves it), and
// the next backup runs.
func TestStoppedBackupLeavesNoLock(t *testing.T) {
	for _, stop := range []string{"cancel", "timeout"} {
		t.Run(stop, func(t *testing.T) {
			f := newFixture(t)
			f.write("locking-backup-files", "")
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			want := context.Canceled
			if stop == "timeout" {
				cctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				want = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			go func() {
				_, err := f.o.RunBackup(cctx, rpc.BackupParams{Site: f.site, Repository: repoURL, Password: "pw", Kind: "files"})
				done <- err
			}()
			waitPids(t, f.dir, "pid")
			if _, err := os.Stat(filepath.Join(f.dir, "lock")); err != nil {
				t.Fatal("control: the stand-in holds no lock while it runs")
			}
			if stop == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("stopped backup = %v, want %v", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("RunBackup did not return within 10 s of being stopped")
			}
			if _, err := os.Stat(filepath.Join(f.dir, "lock")); err == nil {
				t.Fatal("restic was killed before it could remove its lock")
			}
			os.Remove(filepath.Join(f.dir, "locking-backup-files"))
			if _, err := f.backup("files"); err != nil {
				t.Fatalf("the backup after a stopped one: %v", err)
			}
		})
	}
}

func waitPids(t *testing.T, dir string, names ...string) []int {
	t.Helper()
	var pids []int
	deadline := time.Now().Add(10 * time.Second)
	for _, n := range names {
		for {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && perr == nil {
				pids = append(pids, pid)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the restic stand-in never wrote %s", n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return pids
}

// alive reports whether pid runs (a zombie does not).
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	s := string(b)
	if i := strings.LastIndex(s, ") "); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z' && s[i+2] != 'X'
	}
	return true
}

// ------------------------------------------------------- RestoreSnapshot --

// A full restore swaps the snapshot's files in and imports its own database
// snapshot over an emptied database; the site's PHP master is restarted, the
// page cache emptied with its root kept (N-3), nothing left beside the sites.
func TestRestoreFull(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	mk(t, f.site.RootPath, map[string]string{"releases/initial/index.html": "changed since", "releases/initial/new.html": "new"})
	var imported []string
	f.m.OnImport = func(_, sql string) error {
		imported = append(imported, sql)
		return nil
	}
	if err := f.restore("full"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.live("current/index.html"); got != "live" {
		t.Fatalf("index.html = %q, want the snapshot's", got)
	}
	if _, err := os.Stat(filepath.Join(f.site.RootPath, "current/new.html")); err == nil {
		t.Fatal("a file made after the snapshot survived the restore")
	}
	if len(imported) != 1 || !strings.Contains(imported[0], "CREATE TABLE good") {
		t.Fatalf("imported %q, want the database snapshot's dump", imported)
	}
	if !strings.Contains(strings.Join(f.m.Statements(), "\n"), "DROP DATABASE IF EXISTS `site_3`") {
		t.Fatal("the dump was imported on top of the current database")
	}
	if f.restarts != 1 || f.reloads != 1 || f.purges != 1 {
		t.Fatalf("restarts/reloads/purges = %d/%d/%d", f.restarts, f.reloads, f.purges)
	}
	if fi, err := os.Stat(f.cache); err != nil || !fi.IsDir() {
		t.Fatal("the page cache root was removed")
	}
	if ents, _ := os.ReadDir(f.cache); len(ents) != 0 {
		t.Fatal("pre-restore pages stayed in the cache")
	}
	assertNoStage(t, f)
	if os.Getuid() == 0 {
		owners := map[string]int{".": 0, "releases": 0, "releases/initial/index.html": siteUID, "shared/wp-config.php": siteUID}
		for rel, uid := range owners {
			var st syscall.Stat_t
			if err := syscall.Lstat(filepath.Join(f.site.RootPath, rel), &st); err != nil || int(st.Uid) != uid {
				t.Errorf("%s owned by %d, want %d (%v)", rel, st.Uid, uid, err)
			}
		}
	}
}

func assertNoStage(t *testing.T, f *fixture) {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Dir(f.site.RootPath))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".cg-restore-") {
			t.Fatalf("restore staging %s left beside the sites", e.Name())
		}
	}
	work, _ := os.ReadDir(f.o.WorkDir)
	for _, e := range work {
		if e.Name() != "rollback" {
			t.Fatalf("work directory %s left behind", e.Name())
		}
	}
}

// The database comes from the backup's own database snapshot, never from
// the logs/db-latest.sql in the site's files, which the site user can
// write; a snapshot that has only that one is refused in full and database
// mode and still restores in files mode.
func TestRestoreNeverImportsTheSitesOwnDump(t *testing.T) {
	f := newFixture(t)
	mk(t, f.site.RootPath, map[string]string{"logs/db-latest.sql": "-- MariaDB dump\nCREATE DATABASE cg_pwn;"})
	f.seed(false, "")
	for _, mode := range []string{"full", "database"} {
		if err := f.restore(mode); err == nil || !strings.Contains(err.Error(), "files mode") {
			t.Fatalf("%s restore of a snapshot without a database snapshot = %v", mode, err)
		}
	}
	if len(f.m.Imported) != 0 {
		t.Fatal("the dump in the site's files was imported")
	}
	if res := f.verify(); res.Passed {
		t.Fatalf("a backup without its database verified: %+v", res)
	}
	if err := f.restore("files"); err != nil {
		t.Fatalf("files restore of an old snapshot: %v", err)
	}
}

// The db: tag must lead to this site's database snapshot.
func TestRestoreRefusesForeignDatabaseSnapshot(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\n")
	f.write("snap-"+dbID+".json", `[{"id":"`+dbID+`","hostname":"box","tags":["site:other.example.com","part:database"]}]`)
	if err := f.restore("database"); err == nil || len(f.m.Imported) != 0 {
		t.Fatalf("restored another site's database snapshot: %v", err)
	}
	f.write("snap-"+filesID+".json", `[{"id":"`+filesID+`","hostname":"box","tags":["site:docs.example.com","db:../../etc"]}]`)
	if err := f.restore("database"); err == nil || len(f.m.Imported) != 0 {
		t.Fatalf("followed an invalid db: tag: %v", err)
	}
}

// A mode or an ID that is not one is refused before restic runs.
func TestRestoreValidatesBeforeTheRepository(t *testing.T) {
	f := newFixture(t)
	for _, p := range []rpc.RestoreParams{
		{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID, Mode: "everything"},
		{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: "--target=/"},
		{Site: f.site, SnapshotID: filesID},
	} {
		if _, err := f.o.RestoreSnapshot(ctx, p); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v = %v", p, err)
		}
	}
	// A site the agent records no database for.
	noDB := f.site
	noDB.ID, noDB.Config.Database.Enabled = 9, false
	if _, err := f.o.RestoreSnapshot(ctx, rpc.RestoreParams{Site: noDB, Repository: repoURL, Password: "pw", SnapshotID: filesID, Mode: "database"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("database restore without a database = %v", err)
	}
	if f.log() != "" {
		t.Fatalf("restic ran:\n%s", f.log())
	}
}

// A site that does not come up after the swap gets its files and database
// back, and its PHP restarted on them.
func TestRestoreUndoesWhenTheSiteDoesNotComeUp(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	mk(t, f.site.RootPath, map[string]string{"releases/initial/index.html": "current"})
	f.m.Dumps["site_3"] = "-- the database before"
	var last string
	f.m.OnImport = func(_, sql string) error {
		last = sql
		return nil
	}
	f.restartErr = errors.New("php-fpm failed")
	err := f.restore("full")
	if err == nil || !strings.Contains(err.Error(), "php-fpm failed") {
		t.Fatalf("restore = %v", err)
	}
	if got := f.live("current/index.html"); got != "current" {
		t.Fatalf("index.html = %q after the undo, want the files from before", got)
	}
	if got := last; got != "-- the database before" {
		t.Fatalf("database after the undo = %q, want the rollback point", got)
	}
	if f.restarts != 2 || f.reloads != 1 {
		t.Fatalf("restarts/reloads = %d/%d, want PHP restarted on the original files", f.restarts, f.reloads)
	}
	assertNoStage(t, f)
}

// A database restore changes what the running site reads: its PHP is
// refreshed, so an object cache in APCu does not keep serving the options
// from before the restore. Its files are not touched.
func TestDatabaseRestoreRefreshesPHP(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	mk(t, f.site.RootPath, map[string]string{"releases/initial/index.html": "current"})
	if err := f.restore("database"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.m.Imported["site_3"], "CREATE TABLE good") {
		t.Fatalf("imported %q", f.m.Imported)
	}
	if f.refreshes != 1 || f.purges != 1 {
		t.Fatalf("refreshes/purges = %d/%d after a database restore", f.refreshes, f.purges)
	}
	if f.restarts != 0 || f.live("current/index.html") != "current" || strings.Contains(f.log(), "restore ") {
		t.Fatal("a database restore touched the files")
	}
}

// A snapshot whose current link is not a release, or names one it does not
// hold, is refused before anything changes.
func TestRestoreRefusesTreeWithoutLiveRelease(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE good (id int);\n")
	tree := filepath.Join(f.dir, "tree-"+filesID, f.site.RootPath)
	os.RemoveAll(filepath.Join(tree, "releases", "initial"))
	if err := f.restore("full"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("restore of a tree without its live release = %v", err)
	}
	if f.live("current/index.html") != "live" || len(f.m.Imported) != 0 {
		t.Fatal("a refused restore changed the site")
	}
}

// --------------------------------------------------------- VerifyBackup --

// The restore test imports the dump into a throwaway cgv_ database through
// the restore's sandboxed import, counts what came back, drops it always,
// and runs restic's check. A dump that does not import fails.
func TestVerifyImportsIntoScratchDatabase(t *testing.T) {
	f := newFixture(t)
	f.site.Type = state.SiteWordPress
	f.seed(true, "-- MariaDB dump\nCREATE TABLE wp_options (id int);\n")
	var into, sql string
	var importErr error
	f.m.OnImport = func(database, s string) error {
		into, sql = database, s
		return importErr
	}
	tables, options := "12", "1"
	f.m.OnQuery = func(q string, _ []any) ([]string, error) {
		if strings.Contains(q, "options") {
			return []string{options}, nil
		}
		return []string{tables}, nil
	}
	res := f.verify()
	if !res.Passed || !strings.Contains(res.Detail, "12 tables") {
		t.Fatalf("verify = %+v", res)
	}
	if !strings.HasPrefix(into, "cgv_") || !strings.Contains(sql, "wp_options") {
		t.Fatalf("imported into %q (%q), want a cgv_ scratch database", into, sql)
	}
	dropped := func() bool {
		return strings.Contains(strings.Join(f.m.Statements(), "\n"), "DROP DATABASE IF EXISTS `"+into+"`")
	}
	if !dropped() {
		t.Fatal("the scratch database was not dropped")
	}
	if !strings.Contains(f.log(), "check --read-data-subset=10%") {
		t.Fatal("the repository was not checked")
	}

	importErr = errors.New("ERROR 1064 (42000) at line 2: syntax error")
	if res := f.verify(); res.Passed || !strings.Contains(res.Detail, "syntax error") {
		t.Fatalf("a dump that does not import verified: %+v", res)
	}
	if !dropped() {
		t.Fatal("the scratch database stayed after a failed import")
	}
	importErr = nil
	for _, answer := range [][2]string{{"0", "0"}, {"5", "0"}} {
		tables, options = answer[0], answer[1]
		if res := f.verify(); res.Passed {
			t.Fatalf("verify passed with tables/options = %v", answer)
		}
	}
}

// The restore test is never weaker than the restore: a snapshot the restore
// would refuse (no live release in it) fails it. It used to count the files
// and pass.
func TestVerifyFailsWhatRestoreRefuses(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\nCREATE TABLE t (id int);\n")
	f.m.OnQuery = func(string, []any) ([]string, error) { return []string{"3"}, nil }
	if res := f.verify(); !res.Passed {
		t.Fatalf("control: a good backup failed: %+v", res)
	}
	os.RemoveAll(filepath.Join(f.dir, "tree-"+filesID, f.site.RootPath, "releases", "initial"))
	if res := f.verify(); res.Passed {
		t.Fatalf("a snapshot without its live release verified: %+v", res)
	}
	if err := f.restore("full"); err == nil {
		t.Fatal("control: the restore accepted it")
	}
}

// restic's output (which can name the repository's location) stays in the
// log; the panel gets what failed.
func TestVerifyDetailHidesResticOutput(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\n")
	f.write("exit-restore", "1")
	res := f.verify()
	if res.Passed || strings.Contains(res.Detail, repoURL) || !strings.Contains(res.Detail, "restore the snapshot's files") {
		t.Fatalf("detail = %q", res.Detail)
	}
}

// A cancelled restore test is not a verdict on the backup.
func TestVerifyCancelledIsNotAVerdict(t *testing.T) {
	f := newFixture(t)
	f.seed(true, "-- MariaDB dump\n")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if res, err := f.o.VerifyBackup(cctx, rpc.RestoreParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: filesID}); err == nil {
		t.Fatalf("a cancelled verification returned a verdict: %+v", res)
	}
}

func TestLooksLikeSQL(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	if err := looksLikeSQL(write("ok.sql", "-- MariaDB dump 10.19\n"+strings.Repeat("INSERT INTO t VALUES (1);\n", 10000))); err != nil {
		t.Fatalf("a real dump was rejected: %v", err)
	}
	for name, body := range map[string]string{"empty.sql": "", "junk.sql": strings.Repeat("\x00garbage", 1000)} {
		if err := looksLikeSQL(write(name, body)); err == nil {
			t.Fatalf("%s passed as SQL", name)
		}
	}
}

// ------------------------------------------------------ BackupRetention --

const (
	snapNew      = "aaaaaaaa11111111111111111111111111111111111111111111111111111111"
	snapVerified = "bbbbbbbb22222222222222222222222222222222222222222222222222222222"
	snapOld      = "cccccccc33333333333333333333333333333333333333333333333333333333"
)

func TestRetentionForgetsOnlyUnprotectedSnapshots(t *testing.T) {
	f := newFixture(t)
	f.write("plan", "some text first\n"+`[{"host":"box","keep":[{"id":"`+snapNew+`"}],`+
		`"remove":[{"id":"`+snapVerified+`"},{"id":"`+snapOld+`"}]}]`)
	res, err := f.o.BackupRetention(ctx, rpc.RetentionParams{Site: f.site, Repository: repoURL, Password: "pw",
		KeepWithinDays: 30, Protect: []string{"bbbbbbbb"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Forgotten) != 1 || res.Forgotten[0] != snapOld {
		t.Fatalf("forgotten = %v", res.Forgotten)
	}
	if kept := strings.Join(res.Kept, " "); !strings.Contains(kept, snapNew) || !strings.Contains(kept, snapVerified) {
		t.Fatalf("kept = %v", res.Kept)
	}
	calls := strings.Split(strings.TrimSpace(f.log()), "\n")
	if len(calls) != 2 || calls[0] != "forget --dry-run --json --tag site:docs.example.com --host box --keep-within 30d" ||
		calls[1] != "forget --prune "+snapOld {
		t.Fatalf("restic calls = %q", calls)
	}
}

// A group that keeps nothing is not acted on, nothing to forget runs
// nothing, an invalid ID in the plan fails the run.
func TestRetentionRefusesMisreadPlans(t *testing.T) {
	f := newFixture(t)
	p := rpc.RetentionParams{Site: f.site, Repository: repoURL, Password: "pw", KeepWithinDays: 7}
	f.write("plan", `[{"keep":[],"remove":[{"id":"`+snapOld+`"}]}]`)
	if res, err := f.o.BackupRetention(ctx, p); err != nil || len(res.Forgotten) != 0 || strings.Contains(f.log(), "--prune") {
		t.Fatalf("a group with nothing kept was pruned: %+v %v", res, err)
	}
	f.write("plan", `[{"keep":[{"id":"`+snapNew+`"}],"remove":[{"id":"--all"}]}]`)
	if _, err := f.o.BackupRetention(ctx, p); err == nil {
		t.Fatal("an invalid id in the plan was accepted")
	}
	f.write("plan", "no plan")
	if _, err := f.o.BackupRetention(ctx, p); err == nil {
		t.Fatal("an unreadable plan was accepted")
	}
	for _, bad := range []rpc.RetentionParams{
		{Site: f.site, Repository: repoURL, Password: "pw", KeepWithinDays: 0},
		{Site: f.site, Repository: repoURL, Password: "pw", KeepWithinDays: 1, Protect: []string{"x"}},
	} {
		if _, err := f.o.BackupRetention(ctx, bad); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%+v = %v", bad, err)
		}
	}
}

// Every kept files snapshot keeps the database snapshot it names, even when
// restic plans that one away in another group.
func TestRetentionKeepsPairedDatabaseSnapshots(t *testing.T) {
	groups := []host.ForgetGroup{
		{Keep: []host.Snapshot{{ID: "aaaaaaaa11111111", Tags: []string{"db:bbbbbbbb22222222"}}},
			Remove: []host.Snapshot{{ID: "cccccccc33333333", Tags: []string{"db:dddddddd44444444"}}}},
		{Keep: []host.Snapshot{{ID: "eeeeeeee55555555", Tags: []string{"part:database"}}},
			Remove: []host.Snapshot{{ID: "bbbbbbbb22222222", Tags: []string{"part:database"}}, {ID: "dddddddd44444444", Tags: []string{"part:database"}}}},
	}
	res, err := planRetention(groups, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Forgotten, " ") != "cccccccc33333333 dddddddd44444444" {
		t.Fatalf("forgotten = %v, want the old files snapshot and its database only", res.Forgotten)
	}
}

// ------------------------------------------------------------------ lock --

func TestRepoLock(t *testing.T) {
	var l repoLock
	r1, err := l.acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := l.acquire(ctx, false) // shared holders coexist
	got := make(chan func(), 1)
	go func() {
		release, _ := l.acquire(ctx, true)
		got <- release
	}()
	// An exclusive waiter goes before new shared holders.
	for {
		l.mu.Lock()
		w := l.waiting
		l.mu.Unlock()
		if w == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(short, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a shared holder overtook a waiting exclusive one: %v", err)
	}
	select {
	case <-got:
		t.Fatal("exclusive while shared holders remain")
	case <-time.After(20 * time.Millisecond):
	}
	r1()
	r2()
	ex := <-got
	short2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, err := l.acquire(short2, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("two exclusive holders")
	}
	ex()
	if r, err := l.acquire(ctx, false); err != nil {
		t.Fatal("the lock was not released")
	} else {
		r()
	}
}

// SR-T6: a files restore (and every other mode, and the restore test)
// checks the snapshot it swaps in: another site's snapshot, one made on
// another server, or a database part is refused before anything is
// restored, and the live tree stays as it was.
func TestRestoreRefusesAnotherSitesSnapshot(t *testing.T) {
	for name, snap := range map[string]string{
		"another site":   `[{"id":"` + filesID + `","hostname":"box","tags":["site:other.example.com","kind:full"]}]`,
		"another server": `[{"id":"` + filesID + `","hostname":"elsewhere","tags":["site:docs.example.com","kind:full"]}]`,
		"no site tag":    `[{"id":"` + filesID + `","hostname":"box","tags":["kind:full"]}]`,
		"database part":  `[{"id":"` + filesID + `","hostname":"box","tags":["site:docs.example.com","part:database"]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.seed(false, "")
			mk(t, filepath.Join(f.dir, "tree-"+filesID, f.site.RootPath), map[string]string{"shared/planted.php": "<?php // another tenant's"})
			f.write("snap-"+filesID+".json", snap)
			for _, mode := range []string{"files", "full"} {
				err := f.restore(mode)
				var e *agentrpc.Error
				if !errors.As(err, &e) || e.Code != agentrpc.CodeInvalid {
					t.Fatalf("%s restore: %v", mode, err)
				}
			}
			if _, err := os.Stat(filepath.Join(f.site.RootPath, "shared/planted.php")); err == nil {
				t.Fatal("the other snapshot's files went live")
			}
			if log, _ := os.ReadFile(filepath.Join(f.dir, "log")); strings.Contains(string(log), "restore "+filesID) {
				t.Fatalf("restic restored the snapshot: %s", log)
			}
			if res := f.verify(); res.Passed {
				t.Fatalf("the restore test passed it: %+v", res)
			}
		})
	}
}

// A failed restore test's detail never carries a repository's password,
// whatever error it comes from (#48).
func TestVerifyDetailRedactsCredentials(t *testing.T) {
	err := fmt.Errorf("open repository rest:https://backup:repo-secret@backups.example.com/repo: %w", errors.New("connection refused"))
	if d := failDetail(err); strings.Contains(d, "repo-secret") || !strings.Contains(d, "backups.example.com") {
		t.Fatalf("detail = %q", d)
	}
}
