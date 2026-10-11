package staging

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// tableRe is a table name a push may name.
var tableRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// maxPushed bounds the tables of one push.
const maxPushed = 50

// protectedRules is the push policy (SPEC §5): user accounts always; for a
// store, the WordPress tables WooCommerce keeps orders, notes and reviews in
// (by suffix) and WooCommerce's own live tables (by substring). The first
// rule that covers a table gives the reason.
var protectedRules = func() []rpc.TableRule {
	const (
		users  = "user accounts"
		posts  = "live store data (orders, notes, reviews)"
		orders = "live store data (orders, customers, payments)"
	)
	rules := []rpc.TableRule{
		{Match: "suffix", Pattern: "_users", Reason: users},
		{Match: "suffix", Pattern: "_usermeta", Reason: users},
	}
	for _, s := range []string{"_posts", "_postmeta", "_comments", "_commentmeta"} {
		rules = append(rules, rpc.TableRule{Match: "suffix", Pattern: s, Reason: posts, StoresOnly: true})
	}
	for _, s := range []string{
		"_wc_order", "_woocommerce_order", "_woocommerce_sessions", "_actionscheduler_",
		"_wc_customer_lookup", "_woocommerce_payment_token", "_woocommerce_downloadable_product_permissions",
		"_wc_download_log", "_wc_reserved_stock", "_woocommerce_api_keys",
	} {
		rules = append(rules, rpc.TableRule{Match: "contains", Pattern: s, Reason: orders, StoresOnly: true})
	}
	return rules
}()

// ProtectedRules is the push policy as data, for Capabilities.
func ProtectedRules() []rpc.TableRule { return slices.Clone(protectedRules) }

// Protected says why table may never be pushed over production, or ""
// when it may (SPEC §5). store is whether production is a store.
func Protected(table string, store bool) string {
	for _, r := range protectedRules {
		if r.StoresOnly && !store {
			continue
		}
		if r.Protects(table) {
			return r.Reason
		}
	}
	return ""
}

// storeByRecord is what the site record says: a WooCommerce site, or the
// commerce profile, which installs WooCommerce.
func storeByRecord(s state.Site) bool {
	return s.Type == state.SiteWooCommerce || s.Profile == state.ProfileCommerce
}

// isStore says whether production is a store: by its record, or by a
// WooCommerce table in its database (installed by hand).
func (o *Ops) isStore(ctx context.Context, prod state.Site) (bool, error) {
	if storeByRecord(prod) {
		return true, nil
	}
	if !localDB(prod) {
		return false, nil
	}
	names, err := o.DB.TableNames(ctx, prod.Config.Database.Name)
	if err != nil {
		return false, fmt.Errorf("list production's tables: %w", err)
	}
	for _, n := range names {
		n = strings.ToLower(n)
		if strings.Contains(n, "_woocommerce_") || strings.Contains(n, "_wc_") {
			return true, nil
		}
	}
	return false, nil
}

// checkDatabases refuses a pair without two primary databases on this
// server, each recorded as its site's (ops/db SPEC §2). Only the primary
// databases are pushed (SPEC §5).
func (o *Ops) checkDatabases(ctx context.Context, prod, stg state.Site) error {
	if !localDB(prod) || !localDB(stg) {
		return agentrpc.Invalid("both sites need a database on this server")
	}
	if _, err := o.DB.SiteDatabase(ctx, prod, ""); err != nil {
		return err
	}
	_, err := o.DB.SiteDatabase(ctx, stg, "")
	return err
}

// Tables lists staging's tables with the push policy applied (SPEC §5).
func (o *Ops) Tables(ctx context.Context, p rpc.StagingParams) (rpc.StagingTablesResult, error) {
	res := rpc.StagingTablesResult{Tables: []rpc.StagingTable{}}
	if err := pair(p.Production, p.Staging); err != nil {
		return res, err
	}
	if err := o.checkDatabases(ctx, p.Production, p.Staging); err != nil {
		return res, err
	}
	store, err := o.isStore(ctx, p.Production)
	if err != nil {
		return res, err
	}
	res.Database = p.Staging.Config.Database.Name
	names, err := o.DB.TableNames(ctx, res.Database)
	if err != nil {
		return res, fmt.Errorf("list staging's tables: %w", err)
	}
	for _, n := range names {
		why := Protected(n, store)
		res.Tables = append(res.Tables, rpc.StagingTable{Name: n, Protected: why != "", Reason: why})
	}
	return res, nil
}

// Push replaces the selected tables of production's database with
// staging's (SPEC §6).
func (o *Ops) Push(ctx context.Context, p rpc.SyncStagingDBParams) (map[string]any, error) {
	prod, stg := p.Production, p.Staging
	if err := pair(prod, stg); err != nil {
		return nil, err
	}
	if len(p.Tables) == 0 || len(p.Tables) > maxPushed {
		return nil, agentrpc.Invalid("select between 1 and %d database tables", maxPushed)
	}
	seen := map[string]bool{}
	for _, t := range p.Tables {
		if !tableRe.MatchString(t) || seen[t] {
			return nil, agentrpc.Invalid("invalid or duplicate table %q", t)
		}
		seen[t] = true
		if why := Protected(t, storeByRecord(prod)); why != "" {
			return nil, agentrpc.Invalid("table %s holds %s of the live site and is never pushed", t, why)
		}
	}
	unlock, err := o.lockPair(prod, stg)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := o.checkDatabases(ctx, prod, stg); err != nil {
		return nil, err
	}

	present, err := o.DB.TableNames(ctx, stg.Config.Database.Name)
	if err != nil {
		return nil, fmt.Errorf("list staging's tables: %w", err)
	}
	have := map[string]bool{}
	for _, t := range present {
		have[t] = true
	}
	store, err := o.isStore(ctx, prod)
	if err != nil {
		return nil, err
	}
	for _, t := range p.Tables {
		if !have[t] {
			return nil, agentrpc.NotFound("staging has no table %s", t)
		}
		if why := Protected(t, store); why != "" {
			return nil, agentrpc.Invalid("table %s holds %s of the live site and is never pushed", t, why)
		}
	}
	// Production's own indexing setting, put back after the push.
	public := map[string]string{}
	if isWordPress(prod) {
		for _, t := range p.Tables {
			if !strings.HasSuffix(strings.ToLower(t), "_options") {
				continue
			}
			v, ok, err := o.blogPublic(ctx, prod.Config.Database.Name, t)
			if err != nil {
				return nil, err
			}
			if ok {
				public[t] = v
			}
		}
	}

	if err := os.MkdirAll(o.WorkDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(o.WorkDir, "db-push-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	dump := filepath.Join(dir, "selected.sql")
	if err := o.DB.DumpFile(ctx, stg.Config.Database.Name, dump, p.Tables...); err != nil {
		return nil, fmt.Errorf("dump the selected staging tables: %w", err)
	}
	// Over: production's other tables stay. A failure from here on puts
	// production back exactly as it was.
	tx, err := o.DB.Replace(ctx, prod.Config.Database.Name, dump, db.Over)
	if err != nil {
		return nil, fmt.Errorf("import the selected tables: %w", err)
	}
	if isWordPress(prod) {
		extra := append(append([]string{}, p.Tables...), "--precise", "--skip-columns=guid")
		if err := o.rewriteURLs(ctx, prod, stg.Domain, prod.Domain, extra...); err != nil {
			return nil, tx.Rollback(fmt.Errorf("rewrite the pushed addresses: %w", err))
		}
		for t, v := range public {
			if err := o.setBlogPublic(ctx, prod.Config.Database.Name, t, v); err != nil {
				return nil, tx.Rollback(fmt.Errorf("restore production's indexing setting: %w", err))
			}
		}
	}
	tx.Commit()
	if o.Refresh != nil {
		if err := o.Refresh(ctx, prod); err != nil {
			o.log().Warn("the pushed tables are in; refreshing the site's caches failed", "site", prod.Domain, "err", err)
		}
	}
	return map[string]any{"tables": p.Tables, "count": len(p.Tables)}, nil
}

// blogPublic reads the blog_public option of an options table.
// Identifiers are checked; the value is a placeholder.
func (o *Ops) blogPublic(ctx context.Context, database, table string) (string, bool, error) {
	if !db.ValidName(database) || !tableRe.MatchString(table) {
		return "", false, agentrpc.Invalid("invalid database or table name")
	}
	q := fmt.Sprintf("SELECT option_value FROM `%s`.`%s` WHERE option_name = ?", database, table)
	got, err := o.DB.MariaDB.QueryStrings(ctx, q, "blog_public")
	if err != nil {
		return "", false, fmt.Errorf("read production's indexing setting: %w", err)
	}
	if len(got) == 0 {
		return "", false, nil
	}
	return got[0], true, nil
}

func (o *Ops) setBlogPublic(ctx context.Context, database, table, value string) error {
	if !db.ValidName(database) || !tableRe.MatchString(table) {
		return agentrpc.Invalid("invalid database or table name")
	}
	q := fmt.Sprintf("UPDATE `%s`.`%s` SET option_value = ? WHERE option_name = ?", database, table)
	return o.DB.MariaDB.Exec(ctx, q, value, "blog_public")
}
