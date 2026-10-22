package dbstudio

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func wordpress(s state.Site) error {
	if s.Type != state.SiteWordPress && s.Type != state.SiteWooCommerce {
		return agentrpc.Invalid("%s is not a WordPress site", s.Domain)
	}
	return nil
}

var prefixRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// optionsTable finds the site's options table: the prefix from wp-cli,
// the table from the server's own list (SPEC §8).
func (o *Ops) optionsTable(ctx context.Context, site state.Site, s *db.Session) (*table, error) {
	return o.prefixedTable(ctx, site, s, "options")
}

// prefixedTable finds one of the site's core tables ("posts", "options"):
// the prefix from wp-cli, the table from the server's own list.
func (o *Ops) prefixedTable(ctx context.Context, site state.Site, s *db.Session, name string) (*table, error) {
	out, err := o.WP(ctx, site, "db", "prefix")
	if err != nil {
		return nil, fmt.Errorf("read the table prefix: %w", err)
	}
	prefix := strings.TrimSpace(string(out))
	if !prefixRe.MatchString(prefix) {
		return nil, agentrpc.Invalid("the site's table prefix is not one Quarry can use")
	}
	return lookup(ctx, s, prefix+name)
}

// autoloaded are the autoload values WordPress loads on every request
// (yes/no before 6.6, on/off/auto… since).
const autoloaded = "autoload IN ('yes', 'on', 'auto', 'auto-on')"

// Autoload measures what WordPress loads on every request (SPEC §8).
func (o *Ops) Autoload(ctx context.Context, p rpc.DBStudioSiteParams) (rpc.DBStudioAutoloadResult, error) {
	if err := wordpress(p.Site); err != nil {
		return rpc.DBStudioAutoloadResult{}, err
	}
	s, err := o.session(ctx, p.Site, "", true, browseTimeout)
	if err != nil {
		return rpc.DBStudioAutoloadResult{}, err
	}
	defer s.Close()
	t, err := o.optionsTable(ctx, p.Site, s)
	if err != nil {
		return rpc.DBStudioAutoloadResult{}, err
	}
	res := rpc.DBStudioAutoloadResult{Table: t.Name, Top: []rpc.DBAutoloadOption{}}
	sum, err := collect(ctx, s, 1, "SELECT COUNT(*), COALESCE(SUM(LENGTH(option_value)), 0) FROM "+quoteIdent(t.Name)+" WHERE "+autoloaded)
	if err != nil {
		return rpc.DBStudioAutoloadResult{}, sqlErr(s, err)
	}
	if len(sum) == 1 && len(sum[0]) == 2 {
		res.Count, res.Bytes = num(sum[0][0]), num(sum[0][1])
	}
	top, err := collect(ctx, s, 20, "SELECT option_name, LENGTH(option_value), autoload FROM "+quoteIdent(t.Name)+
		" WHERE "+autoloaded+" ORDER BY LENGTH(option_value) DESC LIMIT 20")
	if err != nil {
		return rpc.DBStudioAutoloadResult{}, sqlErr(s, err)
	}
	for _, r := range top {
		if len(r) != 3 {
			continue
		}
		res.Top = append(res.Top, rpc.DBAutoloadOption{Name: str(r[0]), Bytes: num(r[1]), Autoload: str(r[2])})
	}
	return res, nil
}

// checkReplace validates a search-replace term: wp-cli takes it as an
// argument, so a leading '-' would read as an option.
func checkReplace(v string) error {
	if v == "" || len(v) > 1000 || strings.HasPrefix(v, "-") || strings.ContainsFunc(v, unicode.IsControl) {
		return agentrpc.Invalid("the search and replace terms are 1 to 1000 characters, not starting with '-', without control characters")
	}
	return nil
}

// SearchReplace replaces from with to in every table of the site's prefix
// through wp-cli, which keeps serialized PHP values intact (SPEC §8).
func (o *Ops) SearchReplace(ctx context.Context, p rpc.DBStudioSearchReplaceParams) (rpc.DBStudioSearchReplaceResult, error) {
	if err := wordpress(p.Site); err != nil {
		return rpc.DBStudioSearchReplaceResult{}, err
	}
	if err := checkReplace(p.From); err != nil {
		return rpc.DBStudioSearchReplaceResult{}, err
	}
	if err := checkReplace(p.To); err != nil {
		return rpc.DBStudioSearchReplaceResult{}, err
	}
	if p.From == p.To {
		return rpc.DBStudioSearchReplaceResult{}, agentrpc.Invalid("the search and replace terms are the same")
	}
	if p.ReadOnly && !p.DryRun {
		return rpc.DBStudioSearchReplaceResult{}, agentrpc.Invalid("this account can only read")
	}
	args := []string{"search-replace", p.From, p.To, "--all-tables-with-prefix", "--precise", "--skip-columns=guid", "--format=count"}
	if p.DryRun {
		args = append(args, "--dry-run")
	}
	out, err := o.WP(ctx, p.Site, args...)
	if err != nil {
		return rpc.DBStudioSearchReplaceResult{}, err
	}
	n, err := lastNumber(out)
	if err != nil {
		return rpc.DBStudioSearchReplaceResult{}, fmt.Errorf("wp search-replace: %w", err)
	}
	res := rpc.DBStudioSearchReplaceResult{Replacements: n, Executed: !p.DryRun}
	if !p.DryRun {
		o.refresh(ctx, p.Site)
	}
	return res, nil
}

// lastNumber reads the count wp-cli prints last.
func lastNumber(out []byte) (int64, error) {
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return 0, fmt.Errorf("no count in the output")
	}
	return strconv.ParseInt(lines[len(lines)-1], 10, 64)
}

func (o *Ops) refresh(ctx context.Context, s state.Site) {
	if o.Refresh == nil {
		return
	}
	if err := o.Refresh(ctx, s); err != nil {
		o.log().Warn("site caches not refreshed after a database change", "site", s.Domain, "err", err)
	}
}

// expiredQuery counts the expired transients; the table is the server's.
func expiredQuery(table string) string {
	return "SELECT COUNT(*) FROM " + quoteIdent(table) +
		" WHERE (option_name LIKE '!_transient!_timeout!_%' ESCAPE '!' OR option_name LIKE '!_site!_transient!_timeout!_%' ESCAPE '!')" +
		" AND CAST(option_value AS UNSIGNED) < UNIX_TIMESTAMP()"
}

// Transients counts the expired transients and, unless DryRun, deletes
// them through wp-cli (SPEC §8).
func (o *Ops) Transients(ctx context.Context, p rpc.DBStudioTransientsParams) (rpc.DBStudioTransientsResult, error) {
	if err := wordpress(p.Site); err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	if p.ReadOnly && !p.DryRun {
		return rpc.DBStudioTransientsResult{}, agentrpc.Invalid("this account can only read")
	}
	s, err := o.session(ctx, p.Site, "", true, browseTimeout)
	if err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	defer s.Close()
	t, err := o.optionsTable(ctx, p.Site, s)
	if err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	count := func() (int64, error) {
		r, err := collect(ctx, s, 1, expiredQuery(t.Name))
		if err != nil {
			return 0, sqlErr(s, err)
		}
		if len(r) != 1 {
			return 0, nil
		}
		return num(r[0][0]), nil
	}
	before, err := count()
	if err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	res := rpc.DBStudioTransientsResult{Expired: before}
	if p.DryRun {
		return res, nil
	}
	if _, err := o.WP(ctx, p.Site, "transient", "delete", "--expired"); err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	after, err := count()
	if err != nil {
		return rpc.DBStudioTransientsResult{}, err
	}
	res.Deleted, res.Executed = max(before-after, 0), true
	o.refresh(ctx, p.Site)
	return res, nil
}

// The revisions beyond the newest keep of each post (SPEC §8.4): ranked per
// parent, newest first. keep is a number the caller checked (1–100).
func excessRevisions(posts string, keep int, columns string, tail string) string {
	return fmt.Sprintf("SELECT %s FROM (SELECT ID, ROW_NUMBER() OVER (PARTITION BY post_parent ORDER BY post_date DESC, ID DESC) AS rn FROM %s WHERE post_type = 'revision') r WHERE r.rn > %d%s",
		columns, quoteIdent(posts), keep, tail)
}

const (
	// revisionBatch is how many revisions one wp-cli call deletes.
	revisionBatch = 500
	// maxRevisionBatches bounds a run (50 000 revisions); a larger backlog
	// goes on at the next one.
	maxRevisionBatches = 100
)

// Cleanup deletes the site's expired transients and the revisions beyond the
// newest KeepRevisions of each post, and measures the autoloaded options
// (SPEC §8.4). With DryRun it only counts. Deletions go through wp-cli, which
// removes a revision's meta and terms with it; what is reported as deleted
// is the difference between two counts, not what was asked for.
func (o *Ops) Cleanup(ctx context.Context, p rpc.DBStudioCleanupParams) (rpc.DBStudioCleanupResult, error) {
	if err := wordpress(p.Site); err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	if p.KeepRevisions < 1 || p.KeepRevisions > 100 {
		return rpc.DBStudioCleanupResult{}, agentrpc.Invalid("keep between 1 and 100 revisions of each post")
	}
	s, err := o.session(ctx, p.Site, "", true, browseTimeout)
	if err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	defer s.Close()
	posts, err := o.prefixedTable(ctx, p.Site, s, "posts")
	if err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	countRevisions := func() (int64, error) {
		r, err := collect(ctx, s, 1, excessRevisions(posts.Name, p.KeepRevisions, "COUNT(*)", ""))
		if err != nil {
			return 0, sqlErr(s, err)
		}
		if len(r) != 1 {
			return 0, nil
		}
		return num(r[0][0]), nil
	}
	revisions, err := countRevisions()
	if err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	tr, err := o.Transients(ctx, rpc.DBStudioTransientsParams{Site: p.Site, DryRun: p.DryRun})
	if err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	res := rpc.DBStudioCleanupResult{ExpiredTransients: tr.Expired, ExcessRevisions: revisions, DeletedTransients: tr.Deleted, Executed: !p.DryRun}
	if !p.DryRun && revisions > 0 {
		for range maxRevisionBatches {
			rows, err := collect(ctx, s, revisionBatch, excessRevisions(posts.Name, p.KeepRevisions, "r.ID", " ORDER BY r.ID LIMIT "+strconv.Itoa(revisionBatch)))
			if err != nil {
				return rpc.DBStudioCleanupResult{}, sqlErr(s, err)
			}
			if len(rows) == 0 {
				break
			}
			args := []string{"post", "delete"}
			for _, r := range rows {
				// IDs from the server's own answer, never from the caller.
				if id := num(r[0]); id > 0 {
					args = append(args, strconv.FormatInt(id, 10))
				}
			}
			args = append(args, "--force")
			if _, err := o.WP(ctx, p.Site, args...); err != nil {
				return rpc.DBStudioCleanupResult{}, err
			}
			if err := ctx.Err(); err != nil {
				return rpc.DBStudioCleanupResult{}, err
			}
		}
		after, err := countRevisions()
		if err != nil {
			return rpc.DBStudioCleanupResult{}, err
		}
		res.DeletedRevisions = max(revisions-after, 0)
		o.refresh(ctx, p.Site)
	}
	a, err := o.Autoload(ctx, rpc.DBStudioSiteParams{Site: p.Site})
	if err != nil {
		return rpc.DBStudioCleanupResult{}, err
	}
	res.AutoloadCount, res.AutoloadBytes, res.AutoloadTop = a.Count, a.Bytes, a.Top[:min(len(a.Top), 5)]
	if res.AutoloadTop == nil {
		res.AutoloadTop = []rpc.DBAutoloadOption{}
	}
	if !p.DryRun {
		o.logCleanup(p.Site, res, p.KeepRevisions)
	}
	return res, nil
}

// logCleanup appends the run's result to the site's cleanup.log (root's,
// beside its other logs). A failure to write it does not fail the cleanup.
func (o *Ops) logCleanup(s state.Site, r rpc.DBStudioCleanupResult, keep int) {
	if o.LogRoot == "" {
		return
	}
	path := filepath.Join(o.LogRoot, s.Domain, "cleanup.log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		o.log().Warn("could not write the cleanup log", "site", s.Domain, "err", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s cleanup: %d expired transients and %d revisions beyond %d per post deleted; %d autoloaded options, %d bytes\n",
		o.now().UTC().Format(time.RFC3339), r.DeletedTransients, r.DeletedRevisions, keep, r.AutoloadCount, r.AutoloadBytes)
}
