package migration

import (
	"bufio"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/wp"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// lineHead is how much of a dump line is looked at: a CREATE TABLE line is
// short, an extended INSERT can be megabytes and is skipped past.
const lineHead = 512

// createTableRe is a CREATE TABLE statement at the start of a line, as
// mysqldump, mariadb-dump, `wp db export` and phpMyAdmin write it, with the
// table's name (quoted or not, optionally after its database's).
var createTableRe = regexp.MustCompile("(?i)^\\s*CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?(?:(?:`[^`]*`|[A-Za-z0-9_$]+)\\.)?(?:`([^`]+)`|([A-Za-z0-9_$]+))")

// coreTables are the tables only a WordPress installation's main site has,
// all three: a multisite's sub-sites (wp_2_…) have no users table.
var coreTables = []string{"options", "posts", "users"}

// DumpTables lists the tables a dump creates, in order, reading it once
// (SPEC §4.3).
func DumpTables(r io.Reader) ([]string, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var tables []string
	for {
		head, err := readLineHead(br)
		if m := createTableRe.FindSubmatch(head); m != nil {
			name := m[1]
			if name == nil {
				name = m[2]
			}
			tables = append(tables, string(name))
		}
		if errors.Is(err, io.EOF) {
			return tables, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// readLineHead returns the first lineHead bytes of the next line and skips
// the rest of it; at the end of the input it returns io.EOF with the last
// line's head.
func readLineHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(head) < lineHead {
			head = append(head, chunk[:min(len(chunk), lineHead-len(head))]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return head, err
	}
}

// WordPressPrefix is the table prefix the migrated site runs on (SPEC
// §4.3): of the WordPress installations in tables, the one configured names
// (the $table_prefix of the archive's own wp-config.php, "" when it has
// none that can be read), else the only one. No installation, or several
// and no configured choice among them, is invalid: the site would come up
// on tables that are not the migrated ones.
func WordPressPrefix(tables []string, configured string) (string, error) {
	found := wordPressPrefixes(tables)
	switch {
	case configured != "" && slices.Contains(found, configured):
		return configured, nil
	case len(found) == 1:
		return found[0], nil
	case len(found) == 0:
		return "", agentrpc.Invalid("the database dump holds no WordPress tables (a <prefix>options, <prefix>posts and <prefix>users table)")
	case configured != "":
		return "", agentrpc.Invalid("the database dump holds %d WordPress installations (table prefixes %s), none of them the %s the archive's wp-config.php names", len(found), strings.Join(found, ", "), configured)
	}
	return "", agentrpc.Invalid("the database dump holds %d WordPress installations (table prefixes %s) and the archive has no wp-config.php naming one; export only the site's", len(found), strings.Join(found, ", "))
}

// wordPressPrefixes are the p for which p+"options", p+"posts" and
// p+"users" are all in tables, valid prefixes only.
func wordPressPrefixes(tables []string) []string {
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	var found []string
	for _, t := range tables {
		p, ok := strings.CutSuffix(t, coreTables[0])
		if !ok || !wp.ValidTablePrefix(p) || slices.Contains(found, p) {
			continue
		}
		whole := true
		for _, c := range coreTables[1:] {
			whole = whole && have[p+c]
		}
		if whole {
			found = append(found, p)
		}
	}
	return found
}

// dumpTables lists the tables the dump at path creates.
func dumpTables(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DumpTables(f)
}

// archivePrefix is the $table_prefix of the wp-config.php at the root of
// the unpacked site, read as text (ops/wp ParseTablePrefix), or "" when
// there is none to read: no file, a link, too large, not one literal.
func archivePrefix(src *sitefs.Root) string {
	b, err := src.ReadFile(configName, 1<<20)
	if err != nil {
		return ""
	}
	p, err := wp.ParseTablePrefix(b)
	if err != nil {
		return ""
	}
	return p
}

// configName is the site's configuration at the root of its tree.
const configName = "wp-config.php"
