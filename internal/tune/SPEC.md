# tune — MariaDB sized for the machine

`CalculateMariaDB(memTotalMB, cpus, role, commerce)` turns the machine's RAM and CPU count into
MariaDB's tuning; `Render()` writes it as the file at `ConfigPath`
(`/etc/mysql/mariadb.conf.d/99-cloudground.cnf`, a variable only so tests can move it).
`internal/agent/ops/db` calls it with the probed machine (`internal/sysprobe`) and `RoleShared`,
and with `commerce` set when any site is a store.

Every value below is pinned for a matrix of machines by `testdata/*.golden` (`golden_test.go`):
a change is a deliberate golden update, and a change of a default needs A/B numbers
(AGENTS.md rules 5–6).

## 1. Inputs

- `memTotalMB <= 0` is read as 2048, `cpus <= 0` as 2.
- `RoleShared` (also any unknown role): web server, PHP and database on one machine.
  `RoleDedicated`: the machine runs only the database.

## 2. Values

| Setting | Value |
| --- | --- |
| `innodb_buffer_pool_size` | 25 % of RAM shared, 70 % dedicated (integer MB, rounded down), at least 128 MB |
| `innodb_log_file_size` | a quarter of the buffer pool, within 64…2048 MB |
| `max_connections` | 100; 200 from 4096 MB of RAM, 300 from 8192, 500 from 16384 |
| `tmp_table_size` = `max_heap_table_size` | RAM / 32, within 32…256 MB |
| `thread_cache_size` | 8 per CPU |
| `table_open_cache` | 4000 |
| `table_definition_cache` | 2000 (the default 400 is exceeded by a few WooCommerce schemas of about 60 tables, and every query then reloads definitions from disk) |
| `innodb_io_capacity` | 1000 (an SSD baseline) |
| `innodb_flush_log_at_trx_commit` | 2 (at most a second of transactions lost on power failure); 1 when `commerce` (orders and payments need full durability) |
| fixed | `innodb_flush_method = O_DIRECT`, `innodb_file_per_table = 1`, `slow_query_log = 1` to `/var/log/mysql/slow.log` with `long_query_time = 1` |

## 3. The file

A `[mysqld]` section in this order: InnoDB, connections and threads, temporary tables, caches,
slow-query log, security (§4), each under a comment, after a two-line header saying the file is calculated,
managed and watched for drift. Byte-exact text: `testdata/mariadb-samples.golden`.

`String()` is the one-line summary `buffer_pool=<n>M log=<n>M conns=<n> flush=<n>`.

## 4. Security settings

Not tuning: the same on every machine, and the last section of the file under the header
`HardeningHeader`, one line each (`Hardening`):

| Setting | Why |
| --- | --- |
| `local_infile = 0` | `LOAD DATA LOCAL INFILE` in a tenant's SQL would have the client that runs it send a file of the client's machine. The agent's imports run the `mariadb` client as root, so any file root can read (measured on MariaDB 11.8 before the fix: `/root/secret.txt` imported into a table). The agent's own client also passes `--local-infile=0`; this is the server's half, which also covers any other client. |
| `skip-symbolic-links` | `CREATE TABLE … DATA DIRECTORY` / `INDEX DIRECTORY` need no FILE privilege: with symbolic links on (the default, `have_symlink = YES`), a site's account put its table files in any directory the server could write. Off (`have_symlink = DISABLED`), the server ignores the option with warning 1618 and keeps the table in its database's directory. The option name is the same in MariaDB 10.11 (Ubuntu 24.04) and 11.8 (26.04), measured in both. Read at start only: `HardeningNeedsRestart`. |

`HardeningBlock(existing)` is the section for a file that already exists: the header (if the file
lacks it) and the line of every setting whose last occurrence in the file is not that line; `""`
when nothing is missing. The agent appends it to a tuning file written by an older version
(`internal/agent/ops/db` SPEC §7).
