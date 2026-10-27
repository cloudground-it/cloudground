package db

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/sysprobe"
	"github.com/cloudground-it/cloudground/internal/tune"
)

// mariadbRestartTimeout bounds the restart after tuning: past the systemd
// adapter's default ceiling, as MariaDB can take minutes to flush its buffer
// pool and come back.
const mariadbRestartTimeout = 5 * time.Minute

// Exists reports whether the server has a database called name, whoever's
// it is.
func (o *Ops) Exists(ctx context.Context, name string) (bool, error) {
	if !site.ValidDBName(name) {
		return false, agentrpc.Invalid("invalid database name")
	}
	return o.exists(ctx, 0, name)
}

// exists reports whether server (0: the box's own) has a database called name.
func (o *Ops) exists(ctx context.Context, server int64, name string) (bool, error) {
	m, err := o.mariaOfServer(server)
	if err != nil {
		return false, err
	}
	got, err := m.QueryStrings(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name)
	if err != nil {
		return false, fmt.Errorf("look for the database %s: %w", name, err)
	}
	return len(got) == 1 && got[0] == name, nil
}

// TableNames lists the base tables and views of database, as root.
func (o *Ops) TableNames(ctx context.Context, database string) ([]string, error) {
	if !o.known(ctx, database) || scratchNameRe.MatchString(database) {
		return nil, agentrpc.Invalid("invalid database name")
	}
	if pg, ok, err := o.pgOfDatabase(ctx, database); err != nil {
		return nil, err
	} else if ok {
		return tablesPG(ctx, pg, database)
	}
	m, err := o.maria(ctx, database)
	if err != nil {
		return nil, err
	}
	return m.QueryStrings(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", database)
}

func (o *Ops) tuneFile() string {
	if o.TuneFile != "" {
		return o.TuneFile
	}
	return tune.ConfigPath
}

func (o *Ops) machine() (int64, int, error) {
	if o.Probe != nil {
		return o.Probe()
	}
	f, err := sysprobe.Probe("/")
	return f.MemTotalMB, f.CPUCount, err
}

// flushKey is the durability setting a store needs at 1.
const flushKey = "innodb_flush_log_at_trx_commit"

// Tune sizes MariaDB for this machine the first time a database is made,
// and raises durability when a store arrives (SPEC §7). Callers wait here
// while the first tuning restarts the server. A failed first tuning is
// logged, not returned: the server runs on its defaults. A failed
// durability raise is returned.
func (o *Ops) Tune(ctx context.Context, commerce bool) error {
	o.tuneMu.Lock()
	defer o.tuneMu.Unlock()
	path := o.tuneFile()
	cur, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !commerce || configValue(string(cur), flushKey) == "1" {
			return nil
		}
		if err := o.writeTuning(path, true); err != nil {
			return fmt.Errorf("raise MariaDB's durability for a store: %w", err)
		}
		if err := o.MariaDB.Exec(ctx, "SET GLOBAL "+flushKey+" = 1"); err != nil {
			return fmt.Errorf("raise MariaDB's durability for a store: %w", err)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		o.log().Error("mariadb tune: read the tuning file", "file", path, "err", err)
		return nil
	}
	if err := o.writeTuning(path, commerce); err != nil {
		o.log().Error("mariadb tune: not tuned, the server keeps its defaults", "err", err)
		return nil
	}
	if err := o.Systemd.Restart(host.WithJobTimeout(ctx, mariadbRestartTimeout), "mariadb.service"); err != nil {
		o.log().Error("mariadb tune: restart", "err", err)
	}
	if err := o.waitReady(ctx, 60*time.Second); err != nil {
		return fmt.Errorf("MariaDB did not come back after tuning: %w", err)
	}
	return nil
}

// Harden makes the server's security settings hold (SPEC §7): it turns
// local_infile off in the running server, moves the site accounts granted
// on a pattern to their one database (regrant), and appends the settings an
// older version's tuning file lacks (tune SPEC §4). Run at start. A server
// not tuned yet gets them with its first tuning.
//
// When it added a setting the server reads only at start (symbolic links),
// it returns restart, which restarts MariaDB and waits for it; the caller
// runs it in the background, so the agent serves meanwhile (a large buffer
// pool can take minutes to come back).
func (o *Ops) Harden(ctx context.Context) (restart func(), err error) {
	o.tuneMu.Lock()
	defer o.tuneMu.Unlock()
	var errs []error
	if err := o.MariaDB.Exec(ctx, "SET GLOBAL local_infile = 0"); err != nil {
		errs = append(errs, fmt.Errorf("turn local_infile off: %w", err))
	}
	if err := o.regrant(ctx); err != nil {
		errs = append(errs, err)
	}
	path := o.tuneFile()
	cur, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.Join(errs...)
	}
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	content := string(cur)
	add := tune.HardeningBlock(content)
	if add == "" {
		return nil, errors.Join(errs...)
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := writeAtomic(path, content+add, 0o644); err != nil {
		return nil, errors.Join(append(errs, fmt.Errorf("add the security settings to %s: %w", path, err))...)
	}
	o.log().Info("mariadb: security settings added to the tuning file", "file", path)
	if tune.HardeningNeedsRestart(add) {
		restart = o.restartHardened
	}
	return restart, errors.Join(errs...)
}

// restartHardened restarts MariaDB so it reads the settings Harden added,
// under the tuning lock (a database created meanwhile waits for it).
func (o *Ops) restartHardened() {
	o.tuneMu.Lock()
	defer o.tuneMu.Unlock()
	ctx := context.Background()
	o.log().Warn("mariadb: restarting once to apply the security settings; sites lose the database for the restart")
	if err := o.Systemd.Restart(host.WithJobTimeout(ctx, mariadbRestartTimeout), "mariadb.service"); err != nil {
		o.log().Error("mariadb: restart for the security settings", "err", err)
		return
	}
	if err := o.waitReady(ctx, mariadbRestartTimeout); err != nil {
		o.log().Error("mariadb did not come back after the restart for the security settings", "err", err)
		return
	}
	o.log().Info("mariadb restarted with the security settings")
}

// regrant moves every site account still granted on its database's name as
// a pattern (`site_1`.*, as Create wrote it before SR-T2: `_` matches any
// character, so site01, siteA1, … were the account's too, and it could
// create them) to the escaped grant on that one database. The new grant
// comes first, so the site never loses its own database; a second run
// finds nothing (an escaped name no longer equals its account's).
func (o *Ops) regrant(ctx context.Context) error {
	rows, err := o.MariaDB.QueryStrings(ctx, "SELECT CONCAT(User, ' ', Db) FROM mysql.db WHERE Host = 'localhost' AND User = Db")
	if err != nil {
		return fmt.Errorf("list the site accounts' grants: %w", err)
	}
	var errs []error
	for _, row := range rows {
		user, database, _ := strings.Cut(row, " ")
		if user != database || !host.ValidDatabaseName(database) || !strings.ContainsAny(database, "_%") {
			continue
		}
		if err := o.MariaDB.Exec(ctx, "GRANT ALL PRIVILEGES ON `"+host.GrantPattern(database)+"`.* TO ?@'localhost'", user); err != nil {
			errs = append(errs, fmt.Errorf("grant %s its own database: %w", user, err))
			continue
		}
		if err := o.MariaDB.Exec(ctx, "REVOKE ALL PRIVILEGES ON `"+database+"`.* FROM ?@'localhost'", user); err != nil {
			errs = append(errs, fmt.Errorf("revoke %s's pattern grant: %w", user, err))
			continue
		}
		o.log().Info("mariadb: a site account's grant is now on its own database only", "account", user)
	}
	return errors.Join(errs...)
}

// writeTuning renders the tuning for this machine and writes it atomically.
func (o *Ops) writeTuning(path string, commerce bool) error {
	mem, cpus, err := o.machine()
	if err != nil {
		return fmt.Errorf("probe the machine: %w", err)
	}
	cfg := tune.CalculateMariaDB(mem, cpus, tune.RoleShared, commerce)
	content, err := cfg.Render()
	if err != nil {
		return err
	}
	if err := writeAtomic(path, content, 0o644); err != nil {
		return err
	}
	o.log().Info("mariadb tuned for this machine", "config", cfg.String())
	return nil
}

// waitReady waits until the server answers, at most budget.
func (o *Ops) waitReady(ctx context.Context, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		err := o.MariaDB.Exec(ctx, "SELECT 1")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// configValue reads key's value from an option file: the last
// "key = value" line, comments and blanks ignored.
func configValue(content, key string) string {
	val := ""
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.ReplaceAll(strings.TrimSpace(k), "-", "_") == key {
			val = strings.TrimSpace(v)
		}
	}
	return val
}

// writeAtomic replaces path with content, mode exactly. A missing directory
// is made 0755 whatever the agent's umask: MariaDB, which runs as mysql,
// reads its option files there.
func writeAtomic(path, content string, mode os.FileMode) error {
	if err := rootfile.MkdirAllMode(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cloudground-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
