package dbstudio

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	out, err := o.WP(ctx, site, "db", "prefix")
	if err != nil {
		return nil, fmt.Errorf("read the table prefix: %w", err)
	}
	prefix := strings.TrimSpace(string(out))
	if !prefixRe.MatchString(prefix) {
		return nil, agentrpc.Invalid("the site's table prefix is not one Quarry can use")
	}
	return lookup(ctx, s, prefix+"options")
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
