package staging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

type fakeWP struct {
	mu    sync.Mutex
	calls []host.Invocation
	fail  func(args []string) error
}

func (f *fakeWP) Run(_ context.Context, inv host.Invocation) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		return nil, fail(inv.Args)
	}
	return nil, nil
}

// lines are the calls as "user: args".
func (f *fakeWP) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.User+": "+strings.Join(c.Args, " "))
	}
	return out
}

type fixture struct {
	*Ops
	dir       string
	maria     *host.FakeMariaDB
	wp        *fakeWP
	mu        sync.Mutex
	events    []string
	locked    map[string]bool
	prod, stg state.Site
}

func (f *fixture) event(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *fixture) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.events, ",")
}

func owner() sitefs.Owner { return sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()} }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, maria: host.NewFakeMariaDB(), wp: &fakeWP{}, locked: map[string]bool{}}
	// production's and staging's databases, as the agent's record rebuilds
	// them (ops/db SPEC §2).
	f.maria.Rows[db.ScanDatabasesQuery] = []string{"site_3", "site_4"}
	f.maria.Rows[db.ScanUsersQuery] = []string{"site_3", "site_4"}
	f.prod = state.Site{ID: 3, Domain: "shop.example.com", Type: state.SiteWordPress, SystemUser: "cg-site-3",
		RootPath: filepath.Join(dir, "shop.example.com"), PHPVersion: "8.4",
		Config: state.SiteConfig{Database: state.DatabaseConfig{Enabled: true, Name: "site_3", User: "site_3"}}}
	f.stg = f.prod
	f.stg.ID, f.stg.Domain, f.stg.SystemUser, f.stg.StagingOf = 4, "staging.shop.example.com", "cg-site-4", 3
	f.stg.RootPath = filepath.Join(dir, f.stg.Domain)
	f.stg.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_4", User: "site_4"}
	lock := func(domain string) func() {
		f.mu.Lock()
		if f.locked[domain] {
			f.mu.Unlock()
			t.Errorf("%s locked twice", domain)
			return func() {}
		}
		f.locked[domain] = true
		f.events = append(f.events, "lock "+domain)
		f.mu.Unlock()
		return func() {
			f.mu.Lock()
			delete(f.locked, domain)
			f.mu.Unlock()
		}
	}
	work := filepath.Join(dir, "work")
	f.Ops = &Ops{
		Open: func(s state.Site) (*sitefs.Root, error) { return sitefs.Open(s.RootPath, owner()) },
		Lock: lock,
		LockLive: func(s state.Site) (func(), error) {
			if _, err := os.Lstat(s.RootPath); err != nil {
				return nil, agentrpc.Conflict("site %s no longer exists", s.Domain)
			}
			return lock(s.Domain), nil
		},
		Provision: func(_ context.Context, p rpc.CreateSiteParams) (rpc.CreateSiteResult, error) {
			f.event("provision " + p.Site.Domain + " as " + string(p.Site.Type))
			root := p.Site.RootPath
			for _, d := range []string{"releases/initial", "shared/uploads"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
					return rpc.CreateSiteResult{}, err
				}
			}
			os.WriteFile(filepath.Join(root, "releases/initial/index.php"), []byte("placeholder"), 0o644)
			return rpc.CreateSiteResult{SystemUser: p.Site.SystemUser}, os.Symlink("releases/initial", filepath.Join(root, "current"))
		},
		Teardown: func(s state.Site) error {
			f.event("teardown " + s.Domain)
			return os.RemoveAll(s.RootPath)
		},
		Render: func(_ context.Context, s state.Site) ([]rpc.ManagedFile, error) {
			f.event("render " + s.Domain + " as " + string(s.Type))
			return []rpc.ManagedFile{{Path: "/etc/nginx/" + s.Domain + ".conf"}}, nil
		},
		Verify:  func(_ context.Context, s state.Site) error { f.event("verify " + s.Domain); return nil },
		Refresh: func(_ context.Context, s state.Site) error { f.event("refresh " + s.Domain); return nil },
		DB:      &db.Ops{MariaDB: f.maria, WorkDir: work, RegistryFile: filepath.Join(t.TempDir(), "databases.json")},
		WP:      f.wp,
		WorkDir: work,
	}
	// The record is read now, before a test answers the server's queries
	// its own way.
	if err := f.Ops.DB.LoadRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// production builds a WordPress production tree, its release links
// absolute (the layout before R-2.3) when absolute is set.
func (f *fixture) production(t *testing.T, absolute bool) string {
	t.Helper()
	root := f.prod.RootPath
	rel := filepath.Join(root, "releases", "20260101-000000")
	write(t, filepath.Join(rel, "index.php"), "<?php // wordpress")
	write(t, filepath.Join(rel, "wp-content", "themes", "t", "style.css"), "body{}")
	write(t, filepath.Join(root, "shared", "wp-config.php"), "<?php define('DB_NAME', 'site_3');")
	write(t, filepath.Join(root, "shared", "uploads", "2026", "a.jpg"), "jpeg")
	write(t, filepath.Join(root, "shared", "object-cache.php"), "<?php // production's drop-in")
	links := map[string]string{
		"wp-config.php":               "../../shared/wp-config.php",
		"wp-content/uploads":          "../../../shared/uploads",
		"wp-content/object-cache.php": "../../../shared/object-cache.php",
	}
	for link, target := range links {
		if absolute {
			target = filepath.Join(root, "shared", filepath.Base(target))
		}
		if err := os.Symlink(target, filepath.Join(rel, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/20260101-000000", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestPairIsRefusedBeforeAnything(t *testing.T) {
	f := newFixture(t)
	f.production(t, false)
	other := f.stg
	other.StagingOf = 99
	same := f.stg
	same.Domain = f.prod.Domain
	ctx := context.Background()
	for name, stg := range map[string]state.Site{"not its copy": other, "same site": same} {
		calls := map[string]error{}
		_, calls["create"] = f.Create(ctx, rpc.StagingParams{Production: f.prod, Staging: stg})
		_, calls["diff"] = f.Diff(ctx, rpc.StagingParams{Production: f.prod, Staging: stg})
		_, calls["tables"] = f.Tables(ctx, rpc.StagingParams{Production: f.prod, Staging: stg})
		_, calls["push"] = f.Push(ctx, rpc.SyncStagingDBParams{Production: f.prod, Staging: stg, Tables: []string{"wp_options"}})
		for call, err := range calls {
			var e *agentrpc.Error
			if !errors.As(err, &e) || e.Code != agentrpc.CodeInvalid {
				t.Errorf("%s, %s: %v, want invalid", name, call, err)
			}
		}
	}
	if f.got() != "" || len(f.wp.lines()) != 0 {
		t.Fatalf("something ran: %s %v", f.got(), f.wp.lines())
	}
}

func TestProtectedPolicy(t *testing.T) {
	for _, c := range []struct {
		table string
		store bool
		want  bool
	}{
		{"wp_users", false, true},
		{"custom_usermeta", false, true},
		{"WP_USERS", false, true},
		{"wp_options", true, false},
		{"wp_terms", true, false},
		{"wp_posts", false, false},
		{"wp_posts", true, true},
		{"wp_2_postmeta", true, true},
		{"wp_comments", true, true},
		{"wp_wc_orders", true, true},
		{"wp_wc_orders_meta", true, true},
		{"wp_wc_order_stats", true, true},
		{"wp_woocommerce_order_items", true, true},
		{"wp_woocommerce_sessions", true, true},
		{"wp_actionscheduler_actions", true, true},
		{"wp_wc_customer_lookup", true, true},
		{"wp_woocommerce_payment_tokens", true, true},
		{"wp_woocommerce_payment_tokenmeta", true, true},
		{"wp_woocommerce_downloadable_product_permissions", true, true},
		{"wp_wc_download_log", true, true},
		{"wp_wc_reserved_stock", true, true},
		{"wp_woocommerce_api_keys", true, true},
		{"wp_wc_tax_rate_classes", true, false},
		{"wp_woocommerce_shipping_zones", true, false},
		{"wp_wc_orders", false, false},
	} {
		if got := Protected(c.table, c.store) != ""; got != c.want {
			t.Errorf("Protected(%s, store=%v) = %v, want %v", c.table, c.store, got, c.want)
		}
	}
}

// A store is a store whatever its record says: the commerce profile
// installs WooCommerce, and WooCommerce can be installed by hand. The old
// policy looked at the site type only.
func TestStoreIsDetected(t *testing.T) {
	f := newFixture(t)
	f.production(t, false)
	os.MkdirAll(f.stg.RootPath, 0o750)
	f.maria.OnQuery = func(q string, args []any) ([]string, error) {
		switch {
		case strings.Contains(q, "information_schema.TABLES") && args[0] == "site_3":
			return []string{"wp_options", "wp_posts", "wp_wc_orders", "wp_woocommerce_payment_tokens"}, nil
		case strings.Contains(q, "information_schema.TABLES"):
			return []string{"wp_options", "wp_posts", "wp_terms", "wp_wc_orders", "wp_woocommerce_payment_tokens"}, nil
		}
		return nil, nil
	}
	ctx := context.Background()
	res, err := f.Tables(ctx, rpc.StagingParams{Production: f.prod, Staging: f.stg})
	if err != nil {
		t.Fatal(err)
	}
	protected := map[string]bool{}
	for _, tb := range res.Tables {
		protected[tb.Name] = tb.Protected
		if tb.Protected && tb.Reason == "" {
			t.Errorf("%s is protected without a reason", tb.Name)
		}
	}
	for table, want := range map[string]bool{"wp_options": false, "wp_terms": false, "wp_posts": true, "wp_wc_orders": true, "wp_woocommerce_payment_tokens": true} {
		if protected[table] != want {
			t.Errorf("hand-installed store: %s protected = %v, want %v", table, protected[table], want)
		}
	}
	for _, table := range []string{"wp_wc_orders", "wp_woocommerce_payment_tokens", "wp_posts"} {
		if _, err := f.Push(ctx, rpc.SyncStagingDBParams{Production: f.prod, Staging: f.stg, Tables: []string{table}}); err == nil {
			t.Errorf("a hand-installed store pushed %s", table)
		}
	}
	if len(f.maria.Imported) != 0 {
		t.Fatalf("a refused push imported: %v", f.maria.Imported)
	}

	// The commerce profile is a store before the database is asked.
	queried := false
	f.maria.OnQuery = func(string, []any) ([]string, error) { queried = true; return nil, nil }
	prod := f.prod
	prod.Profile = state.ProfileCommerce
	if _, err := f.Push(ctx, rpc.SyncStagingDBParams{Production: prod, Staging: f.stg, Tables: []string{"wp_postmeta"}}); err == nil {
		t.Fatal("a commerce-profile site pushed its postmeta")
	}
	if queried {
		t.Fatal("the database was asked before a statically protected table was refused")
	}
}

// pushFixture is a WordPress production and staging with the tables of a
// push in place: production's blog_public is 1.
func pushFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.production(t, false)
	os.MkdirAll(f.stg.RootPath, 0o750)
	f.maria.Dumps["site_4"] = "-- staging tables\n"
	f.maria.Dumps["site_3"] = "-- production, whole\n"
	f.maria.OnQuery = func(q string, args []any) ([]string, error) {
		switch {
		case strings.Contains(q, "information_schema.TABLES"):
			return []string{"wp_options", "wp_terms"}, nil
		case strings.Contains(q, "`site_3`.`wp_options`") && args[0] == "blog_public":
			return []string{"1"}, nil
		}
		return nil, nil
	}
	return f
}

// A push rewrites staging's address in both forms, puts production's
// indexing setting back, and refreshes production's caches.
func TestPushRewritesKeepsIndexingAndRefreshes(t *testing.T) {
	f := pushFixture(t)
	res, err := f.Push(context.Background(), rpc.SyncStagingDBParams{Production: f.prod, Staging: f.stg, Tables: []string{"wp_options", "wp_terms"}})
	if err != nil {
		t.Fatal(err)
	}
	if res["count"] != 2 {
		t.Errorf("result = %v", res)
	}
	if f.maria.Imported["site_3"] != "-- staging tables\n" {
		t.Fatalf("production imported %q", f.maria.Imported["site_3"])
	}
	want := []string{
		`cg-site-3: search-replace //staging.shop.example.com //shop.example.com wp_options wp_terms --precise --skip-columns=guid`,
		`cg-site-3: search-replace \/\/staging.shop.example.com \/\/shop.example.com wp_options wp_terms --precise --skip-columns=guid`,
	}
	if got := f.wp.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("wp-cli:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	restored := false
	for _, s := range f.maria.Statements() {
		if strings.HasPrefix(s, "UPDATE `site_3`.`wp_options` SET option_value = ? WHERE option_name = ? [1 blog_public]") {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("production's blog_public was not put back: %v", f.maria.Statements())
	}
	if !strings.Contains(f.got(), "refresh shop.example.com") {
		t.Fatalf("production was not refreshed: %s", f.got())
	}
	if !strings.HasPrefix(f.got(), "lock shop.example.com,lock staging.shop.example.com,") {
		t.Fatalf("locks not taken production first: %s", f.got())
	}
	ents, _ := os.ReadDir(f.WorkDir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "db-push-") {
			t.Errorf("the dump was left in the work directory: %s", e.Name())
		}
	}
}

// A failed rewrite puts production's database back exactly, and nothing
// is refreshed.
func TestPushRollsBackOnFailedRewrite(t *testing.T) {
	f := pushFixture(t)
	f.wp.fail = func(args []string) error {
		if args[0] == "search-replace" {
			return errors.New("wp-cli died")
		}
		return nil
	}
	_, err := f.Push(context.Background(), rpc.SyncStagingDBParams{Production: f.prod, Staging: f.stg, Tables: []string{"wp_terms"}})
	if err == nil || !strings.Contains(err.Error(), "wp-cli died") {
		t.Fatalf("err = %v", err)
	}
	if got := f.maria.Imported["site_3"]; got != "-- staging tables\n-- production, whole\n" {
		t.Fatalf("production was not rolled back: imports %q", got)
	}
	dropped := false
	for _, s := range f.maria.Statements() {
		dropped = dropped || s == "DROP DATABASE IF EXISTS `site_3`"
	}
	if !dropped || strings.Contains(f.got(), "refresh") {
		t.Fatalf("rollback was not exact or a refresh ran: %v %s", f.maria.Statements(), f.got())
	}
}

func TestPushNeedsTheTablesOnStaging(t *testing.T) {
	f := pushFixture(t)
	_, err := f.Push(context.Background(), rpc.SyncStagingDBParams{Production: f.prod, Staging: f.stg, Tables: []string{"wp_links"}})
	var e *agentrpc.Error
	if !errors.As(err, &e) || e.Code != agentrpc.CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
	for _, bad := range [][]string{nil, {"wp_terms", "wp_terms"}, {"wp terms"}, {"`x`"}} {
		if _, err := f.Push(context.Background(), rpc.SyncStagingDBParams{Production: f.prod, Staging: f.stg, Tables: bad}); err == nil {
			t.Errorf("accepted tables %q", bad)
		}
	}
}

// A WordPress clone: files and uploads copied, the links relative into
// staging's own shared/ even when production's were absolute, staging's
// own configuration and drop-in, the database copied, secrets on stdin.
func TestCreateClonesWordPress(t *testing.T) {
	f := newFixture(t)
	f.production(t, true)
	f.maria.Dumps["site_3"] = "-- production\n"
	prodCfg := filepath.Join(f.prod.RootPath, "shared", "wp-config.php")
	before, _ := os.ReadFile(prodCfg)
	res, err := f.Create(context.Background(), rpc.StagingParams{Production: f.prod, Staging: f.stg, DBPassword: "db-secret", ConnectorToken: "tok-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Errorf("rendered files not returned: %+v", res)
	}
	want := "lock shop.example.com,lock staging.shop.example.com,provision staging.shop.example.com as php," +
		"render staging.shop.example.com as wordpress,verify staging.shop.example.com"
	if f.got() != want {
		t.Fatalf("steps:\n%s\nwant\n%s", f.got(), want)
	}
	rel := filepath.Join(f.stg.RootPath, "releases", "initial")
	if b, _ := os.ReadFile(filepath.Join(rel, "index.php")); string(b) != "<?php // wordpress" {
		t.Fatalf("release not cloned: %q", b)
	}
	for link, want := range map[string]string{
		"wp-config.php":               filepath.Join(f.stg.RootPath, "shared", "wp-config.php"),
		"wp-content/uploads":          filepath.Join(f.stg.RootPath, "shared", "uploads"),
		"wp-content/object-cache.php": filepath.Join(f.stg.RootPath, "shared", "object-cache.php"),
	} {
		if target, _ := os.Readlink(filepath.Join(rel, link)); filepath.IsAbs(target) {
			t.Errorf("%s is still absolute: %s", link, target)
		}
		if real, err := filepath.EvalSymlinks(filepath.Join(rel, link)); err != nil || real != want {
			t.Errorf("%s resolves to %q (%v), want %q", link, real, err, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.stg.RootPath, "shared", "uploads", "2026", "a.jpg")); string(b) != "jpeg" {
		t.Error("uploads not cloned")
	}
	if b, _ := os.ReadFile(filepath.Join(f.stg.RootPath, "shared", "object-cache.php")); string(b) != connector.ObjectCacheAPCu {
		t.Error("staging did not get its own drop-in")
	}
	if fi, err := os.Lstat(filepath.Join(f.stg.RootPath, "shared", "wp-config.php")); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staging's wp-config.php: %v %v", fi, err)
	}
	if after, _ := os.ReadFile(prodCfg); string(after) != string(before) {
		t.Fatal("production's wp-config.php changed")
	}
	if f.maria.Imported["site_4"] != "-- production\n" {
		t.Fatalf("staging's database got %q", f.maria.Imported["site_4"])
	}

	lines := strings.Join(f.wp.lines(), "\n")
	for _, want := range []string{
		"cg-site-4: config set DB_NAME site_4",
		"cg-site-4: config set DB_USER site_4",
		"cg-site-4: config set DB_PASSWORD --prompt",
		"cg-site-4: config set WP_ENVIRONMENT_TYPE staging",
		"cg-site-4: config set CLOUDGROUND_SITE_TOKEN --prompt",
		"cg-site-4: config shuffle-salts",
		"cg-site-4: search-replace //shop.example.com //staging.shop.example.com --all-tables --precise",
		`cg-site-4: search-replace \/\/shop.example.com \/\/staging.shop.example.com --all-tables --precise`,
		"cg-site-4: option update blog_public 0",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing wp-cli call %q in\n%s", want, lines)
		}
	}
	for _, c := range f.wp.calls {
		if c.User != "cg-site-4" || c.Path != filepath.Join(f.stg.RootPath, "current") {
			t.Errorf("wp-cli ran as %s on %s", c.User, c.Path)
		}
		if strings.Contains(strings.Join(c.Args, " "), "secret") {
			t.Errorf("a secret reached argv: %v", c.Args)
		}
	}
	stdin := map[string]string{}
	for _, c := range f.wp.calls {
		if c.Stdin != "" {
			stdin[c.Args[2]] = c.Stdin
		}
	}
	if stdin["DB_PASSWORD"] != "db-secret" || stdin["CLOUDGROUND_SITE_TOKEN"] != "tok-secret" || len(stdin) != 2 {
		t.Fatalf("secrets on stdin: %v", stdin)
	}
}

// Without a token of its own, a copy that has production's loses it; a
// failed step removes the half-made copy.
func TestCreateWithoutTokenAndRollback(t *testing.T) {
	f := newFixture(t)
	f.production(t, false)
	f.wp.fail = func(args []string) error {
		if args[0] == "config" && args[1] == "shuffle-salts" {
			return errors.New("salts failed")
		}
		return nil
	}
	_, err := f.Create(context.Background(), rpc.StagingParams{Production: f.prod, Staging: f.stg, DBPassword: "pw"})
	if err == nil || !strings.Contains(err.Error(), "salts failed") {
		t.Fatalf("err = %v", err)
	}
	lines := strings.Join(f.wp.lines(), "\n")
	if !strings.Contains(lines, "config has CLOUDGROUND_SITE_TOKEN") || !strings.Contains(lines, "config delete CLOUDGROUND_SITE_TOKEN") {
		t.Fatalf("production's token was not removed:\n%s", lines)
	}
	if !strings.HasSuffix(f.got(), "teardown staging.shop.example.com") {
		t.Fatalf("the half-made copy was not removed: %s", f.got())
	}
	if _, err := os.Lstat(f.stg.RootPath); !os.IsNotExist(err) {
		t.Fatal("staging's tree is still there")
	}
}

func TestCreateRefusals(t *testing.T) {
	f := newFixture(t)
	f.production(t, false)
	prod := f.prod
	prod.Config.Database.External = true
	if _, err := f.Create(context.Background(), rpc.StagingParams{Production: prod, Staging: f.stg}); err == nil {
		t.Fatal("staged a WordPress site on an external database")
	}
	// A link planted at production's wp-config.php fails the clone, and
	// nothing of root's reading reaches staging.
	cfg := filepath.Join(f.prod.RootPath, "shared", "wp-config.php")
	os.Remove(cfg)
	if err := os.Symlink("/etc/passwd", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(context.Background(), rpc.StagingParams{Production: f.prod, Staging: f.stg}); err == nil {
		t.Fatal("cloned through a planted wp-config.php link")
	}
	if !strings.HasSuffix(f.got(), "teardown staging.shop.example.com") {
		t.Fatalf("not rolled back: %s", f.got())
	}
	if len(f.wp.lines()) != 0 {
		t.Fatalf("wp-cli ran: %v", f.wp.lines())
	}
}

// The diff compares content, not times; skips uploads, .git and links.
func TestDiff(t *testing.T) {
	f := newFixture(t)
	prodRel := f.production(t, false)
	stgRel := filepath.Join(f.stg.RootPath, "releases", "initial")
	write(t, filepath.Join(stgRel, "index.php"), "<?php // wordpress") // same, newer
	write(t, filepath.Join(stgRel, "wp-content", "themes", "t", "style.css"), "body{x}")
	write(t, filepath.Join(stgRel, ".git", "HEAD"), "ref")
	write(t, filepath.Join(stgRel, "added.php"), "<?php")
	write(t, filepath.Join(prodRel, "same-size.txt"), "aaaa")
	write(t, filepath.Join(stgRel, "same-size.txt"), "bbbb")
	os.Symlink("index.php", filepath.Join(stgRel, "link.php"))
	os.MkdirAll(filepath.Join(stgRel, "wp-content", "uploads"), 0o755)
	write(t, filepath.Join(stgRel, "wp-content", "uploads", "x.jpg"), "x")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(prodRel, "index.php"), old, old)
	os.Chtimes(filepath.Join(prodRel, "same-size.txt"), old, old)
	os.Symlink("releases/initial", filepath.Join(f.stg.RootPath, "current"))

	d, err := f.Diff(context.Background(), rpc.StagingParams{Production: f.prod, Staging: f.stg})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(d.Added) != "[added.php]" || len(d.Removed) != 0 ||
		fmt.Sprint(d.Modified) != "[same-size.txt wp-content/themes/t/style.css]" || d.Truncated {
		t.Fatalf("diff = %+v", d)
	}
}
