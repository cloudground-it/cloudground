// Package tune sizes MariaDB for the machine it runs on (SPEC.md): the
// buffer pool above all, from the RAM and what else shares the machine.
package tune

import (
	"fmt"
	"strings"
)

// Role is what the machine runs besides MariaDB.
type Role string

const (
	// RoleShared: web server, PHP and the database on one machine.
	RoleShared Role = "shared"
	// RoleDedicated: a machine for the database alone.
	RoleDedicated Role = "dedicated"
)

// ConfigPath is the tuning file MariaDB reads. A variable for tests only.
var ConfigPath = "/etc/mysql/mariadb.conf.d/99-cloudground.cnf"

// MariaDBConfig is one machine's tuning.
type MariaDBConfig struct {
	BufferPoolMB     int64
	LogFileMB        int64
	MaxConnections   int
	TmpTableMB       int64
	TableOpenCache   int
	ThreadCacheSize  int
	IOCapacity       int
	FlushLogAtCommit int // 1 full durability (stores), 2 at most a second lost
}

// clamp bounds v to lo…hi.
func clamp(v, lo, hi int64) int64 { return min(max(v, lo), hi) }

// connectionsFor steps max_connections up with the RAM (SPEC §2).
func connectionsFor(memMB int64) int {
	switch {
	case memMB >= 16384:
		return 500
	case memMB >= 8192:
		return 300
	case memMB >= 4096:
		return 200
	}
	return 100
}

// CalculateMariaDB computes the tuning for memTotalMB of RAM and cpus CPUs
// (SPEC §1–2).
func CalculateMariaDB(memTotalMB int64, cpus int, role Role, commerce bool) MariaDBConfig {
	if memTotalMB <= 0 {
		memTotalMB = 2048
	}
	if cpus <= 0 {
		cpus = 2
	}
	share := int64(25)
	if role == RoleDedicated {
		share = 70
	}
	pool := max(memTotalMB*share/100, 128)
	durability := 2
	if commerce {
		durability = 1
	}
	return MariaDBConfig{
		BufferPoolMB:     pool,
		LogFileMB:        clamp(pool/4, 64, 2048),
		MaxConnections:   connectionsFor(memTotalMB),
		TmpTableMB:       clamp(memTotalMB/32, 32, 256),
		TableOpenCache:   4000,
		ThreadCacheSize:  8 * cpus,
		IOCapacity:       1000,
		FlushLogAtCommit: durability,
	}
}

// Render writes the tuning file (SPEC §3).
func (c MariaDBConfig) Render() (string, error) {
	var b strings.Builder
	put := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	put("# Managed by CloudGround — calculated for this machine, do not edit.")
	put("# An edit is reported as drift: change the tuning in the panel.")
	put("")
	put("[mysqld]")
	put("# InnoDB")
	put("innodb_buffer_pool_size = %dM", c.BufferPoolMB)
	put("innodb_log_file_size = %dM", c.LogFileMB)
	put("innodb_flush_log_at_trx_commit = %d", c.FlushLogAtCommit)
	put("innodb_flush_method = O_DIRECT")
	put("innodb_io_capacity = %d", c.IOCapacity)
	put("innodb_file_per_table = 1")
	put("")
	put("# Clients and the threads that serve them")
	put("max_connections = %d", c.MaxConnections)
	put("thread_cache_size = %d", c.ThreadCacheSize)
	put("")
	put("# In-memory temporary tables")
	put("tmp_table_size = %dM", c.TmpTableMB)
	put("max_heap_table_size = %dM", c.TmpTableMB)
	put("")
	put("# Caches. The definition cache defaults to 400 tables: a few WooCommerce")
	put("# schemas (about 60 tables each) exceed it and every query then reloads table")
	put("# definitions from disk.")
	put("table_open_cache = %d", c.TableOpenCache)
	put("table_definition_cache = 2000")
	put("")
	put("# Slow queries are logged for the panel's tuning advice")
	put("slow_query_log = 1")
	put("slow_query_log_file = /var/log/mysql/slow.log")
	put("long_query_time = 1")
	b.WriteString(HardeningBlock(""))
	return b.String(), nil
}

// HardeningHeader opens the security settings at the end of the file.
const HardeningHeader = "# Security: the same on every machine, not tuning."

// Hardening is the security settings, in order: option, then the line
// written for it. They are not tuning and do not depend on the machine.
var Hardening = []struct{ Key, Line string }{
	// LOAD DATA LOCAL INFILE: a statement in a tenant's dump would make the
	// client running it (as root, during an import) send any file it can
	// read.
	{"local_infile", "local_infile = 0"},
	// CREATE TABLE … DATA/INDEX DIRECTORY needs no FILE privilege: with
	// symbolic links on, a site's account put table files in any directory
	// the server can write. Off, the option is ignored with a warning. Read
	// at start only (the agent restarts the server when it adds it).
	{"symbolic_links", "skip-symbolic-links"},
}

// HardeningNeedsRestart reports whether a block from HardeningBlock holds a
// setting the server reads only at start.
func HardeningNeedsRestart(block string) bool {
	return strings.Contains(block, "skip-symbolic-links")
}

// HardeningBlock is the security section for a file whose content is
// existing: the header and the lines of the settings it lacks ("" when it
// has them all). For a new file, existing is "".
func HardeningBlock(existing string) string {
	var lines []string
	for _, h := range Hardening {
		if !hasSetting(existing, h.Key, h.Line) {
			lines = append(lines, h.Line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	if !strings.Contains(existing, HardeningHeader) {
		b.WriteString(HardeningHeader + "\n")
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// hasSetting reports whether the last line of an option file that sets key
// (dashes and underscores alike, a skip- prefix included) is want.
func hasSetting(content, key, want string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ReplaceAll(s, "-", "_")), " ")
	}
	last := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, _, _ := strings.Cut(line, "=")
		if n := norm(k); n == key || n == "skip_"+key {
			last = norm(strings.ReplaceAll(line, "=", " = "))
		}
	}
	return last == norm(strings.ReplaceAll(want, "=", " = "))
}

// String is the one-line summary of the tuning.
func (c MariaDBConfig) String() string {
	return fmt.Sprintf("buffer_pool=%dM log=%dM conns=%d flush=%d",
		c.BufferPoolMB, c.LogFileMB, c.MaxConnections, c.FlushLogAtCommit)
}
