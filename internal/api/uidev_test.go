//go:build uidev

package api

// A local panel for looking at the UI and measuring what it asks for: the
// real API and store, a fake agent, the built UI from a directory. Not part
// of any test run (build tag uidev).
//
//	CG_UIDEV_ADDR=0.0.0.0:5252 CG_UIDEV_UI=ui/dist \
//	  go test -tags uidev -run TestUIDevServer -timeout 0 ./internal/api/
//
// Sign in as admin@example.com / dev-password-123. Besides the API it
// answers GET /__count (API requests per method and path since the last
// reset; ?reset=1 resets) and POST /__poke?site=<id> (flips the site's
// status between active and error, as a provisioning or a failure would).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestUIDevServer(t *testing.T) {
	addr := os.Getenv("CG_UIDEV_ADDR")
	if addr == "" {
		t.Skip("CG_UIDEV_ADDR not set")
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agent := &fakeAgent{errs: map[string]error{}, results: map[string]any{
		agentrpc.SystemStatus.Name(): rpc.SystemStatusResult{CPUCount: 4, Load1: 0.4, MemTotalMB: 8192, MemAvailableMB: 5000,
			DiskTotalMB: 80000, DiskFreeMB: 52000, NginxRunning: true, MariaDBRunning: true, CPUHeadroomPct: 88, MemHeadroomPct: 61, DiskHeadroomPct: 65},
		agentrpc.ServiceStatus.Name(): rpc.ServiceStatusResult{Services: []rpc.ServiceInfo{
			{Name: "nginx", Unit: "nginx.service", Active: true, Enabled: true, State: "running", MemoryBytes: 40 << 20, Since: now.Add(-48 * time.Hour), Version: "1.28"},
			{Name: "mariadb", Unit: "mariadb.service", Active: true, Enabled: true, State: "running", MemoryBytes: 300 << 20, Since: now.Add(-48 * time.Hour), Version: "11.4"},
		}},
		agentrpc.CertStatus.Name():   rpc.CertStatusResult{Certs: map[string]rpc.CertInfo{}},
		agentrpc.CreateSite.Name():   rpc.CreateSiteResult{SystemUser: "cg-site-9", RootPath: "/srv/sites/new"},
		agentrpc.Capabilities.Name(): testCapabilities,
		// One listing for every folder: enough for the file manager and the
		// release form's checkouts under src/.
		agentrpc.ListFiles.Name(): rpc.ListFilesResult{Entries: []rpc.FileEntry{
			{Name: "shop-theme", IsDir: true, Mode: "drwxr-x---", ModTime: now.Add(-3 * time.Hour).Format(time.RFC3339)},
			{Name: "README.md", Size: 2048, Mode: "-rw-r-----", ModTime: now.Add(-26 * time.Hour).Format(time.RFC3339)},
		}},
		// Quarry (/quarry): enough of a WooCommerce database for its pages.
		agentrpc.DBStudioTables.Name(): rpc.DBStudioTablesResult{Database: "site_2", Tables: []rpc.DBStudioTable{
			{Name: "wp_options", Type: "table", Engine: "InnoDB", Collation: "utf8mb4_unicode_ci", Rows: 1423, DataBytes: 2 << 20, IndexBytes: 512 << 10},
			{Name: "wp_posts", Type: "table", Engine: "InnoDB", Collation: "utf8mb4_unicode_ci", Rows: 5120, DataBytes: 48 << 20, IndexBytes: 6 << 20},
			{Name: "wp_wc_orders", Type: "table", Engine: "InnoDB", Collation: "utf8mb4_unicode_ci", Rows: 860, DataBytes: 3 << 20, IndexBytes: 1 << 20},
			{Name: "v_recent_orders", Type: "view"},
		}, Routines: []rpc.DBRoutine{{Name: "wc_restock", Type: "procedure"}},
			Columns: map[string][]string{"wp_posts": {"ID", "post_title", "post_status", "post_date"}}},
		agentrpc.DBStudioStructure.Name(): rpc.DBStudioStructure{Table: "wp_posts", Type: "table", PrimaryKey: []string{"ID"},
			Columns: []rpc.DBColumn{
				{Name: "ID", Type: "bigint(20) unsigned", DataType: "bigint", Key: "PRI", Extra: "auto_increment"},
				{Name: "post_title", Type: "text", DataType: "text", Collation: "utf8mb4_unicode_ci"},
				{Name: "post_status", Type: "varchar(20)", DataType: "varchar", Key: "MUL", Default: strPtr("publish")},
				{Name: "post_date", Type: "datetime", DataType: "datetime"},
			},
			Indexes:     []rpc.DBIndex{{Name: "PRIMARY", Unique: true, Type: "BTREE", Columns: []rpc.DBIndexColumn{{Name: "ID"}}}},
			ForeignKeys: []rpc.DBForeignKey{}, CreateSQL: "CREATE TABLE `wp_posts` (\n  `ID` bigint(20) unsigned NOT NULL AUTO_INCREMENT,\n  PRIMARY KEY (`ID`)\n)"},
		agentrpc.DBStudioRows.Name(): rpc.DBStudioRowsResult{
			Columns: []rpc.DBColumn{
				{Name: "ID", Type: "bigint(20) unsigned", DataType: "bigint", Key: "PRI"},
				{Name: "post_title", Type: "text", DataType: "text"},
				{Name: "post_status", Type: "varchar(20)", DataType: "varchar"},
				{Name: "post_date", Type: "datetime", DataType: "datetime"},
			},
			PrimaryKey: []string{"ID"}, Total: 5120, Page: 0, PageSize: 25, Rows: uidevRows(25),
			SQL: "SELECT `ID`, LEFT(`post_title`, 4097) AS `post_title`, `post_status`, `post_date` FROM `wp_posts` ORDER BY `ID` LIMIT 25 OFFSET 0"},
		agentrpc.DBStudioFiles.Name(): rpc.DBStudioFilesResult{
			Exports: []rpc.DBStudioFile{{Name: "site_2-20261010-034000.sql", Size: 117 << 20, Modified: now.Add(-6 * time.Hour)}},
			Imports: []rpc.DBStudioFile{{Name: "orders-fix.sql", Size: 48 << 10, Modified: now.Add(-2 * time.Hour)}}},
		agentrpc.DBStudioSearch.Name(): rpc.DBStudioSearchResult{Searched: 4, Tables: []rpc.DBStudioSearchHit{{Table: "wp_posts", Matches: 12}, {Table: "wp_options", Matches: 2}}},
		agentrpc.DBStudioQuery.Name():  rpc.DBQueryResult{Columns: []string{"ID", "post_title"}, Rows: [][]string{{"12", "Linen shirt"}, {"13", "Wool scarf"}}},
		agentrpc.DBStudioSimulate.Name(): rpc.DBStudioSimulateResult{Kind: "update", Affected: 4, RolledBack: true,
			PreviewSQL: "SELECT * FROM wp_posts WHERE ID < 20",
			Preview: &rpc.DBQueryResult{Columns: []string{"ID", "post_title", "post_status"}, Rows: [][]string{
				{"12", "Linen shirt 12", "publish"}, {"13", "Wool scarf 13", "draft"}, {"14", "Cotton tee 14", "trash"}, {"15", "Denim jacket 15", "publish"}}}},
	}}
	s := &Server{Store: store, Agent: agent, Log: slog.Default(), InsecureCookies: true}
	if dir := os.Getenv("CG_UIDEV_UI"); dir != "" {
		s.UI = os.DirFS(dir)
	}
	s.Init()
	hash, _ := hashPassword("dev-password-123")
	admin, err := store.CreateUser("admin@example.com", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	store.CreateAPIToken(admin.ID, "ci-deploy", "cgt_devtoken-one", "cgt_devtok", now.Add(90*24*time.Hour))
	store.CreateAPIToken(admin.ID, "backup-monitor", "cgt_devtoken-two", "cgt_devtwo", time.Time{})
	store.UpdatePanelSettings(func(p *state.PanelSettings) error {
		p.UpdateCheck = false
		p.BackupRepository = "s3:https://r2.example.com/cloudground"
		return nil
	})
	for i, d := range []string{"example.com", "shop.example.com", "blog.example.com"} {
		rec := state.Site{Domain: d, Type: state.SiteStatic, Profile: state.ProfileBalanced,
			Engine: state.EngineNginx, SystemUser: "cg-site-" + strconv.Itoa(i+1), RootPath: "/srv/sites/" + d, Status: state.SiteActive}
		if d == "shop.example.com" {
			// A store, so the PHP, database and WordPress pages have a site.
			rec.Type, rec.Profile, rec.PHPVersion = state.SiteWooCommerce, state.ProfileCommerce, "8.4"
			rec.Config.Database = state.DatabaseConfig{Enabled: true, Name: "site_2", User: "site_2"}
			rec.Config.Backups = state.BackupPolicy{Enabled: true, Schedule: "daily", RetentionDays: 30}
			rec.Config.Resources.MemoryHighMB = 2048
		}
		site, err := store.CreateSite(rec)
		if err != nil {
			t.Fatal(err)
		}
		if d == "shop.example.com" {
			seedShop(t, store, site, now)
		}
		store.SetCertExpiry(site.ID, now.Add(60*24*time.Hour))
		b, _ := store.CreateBackup(state.Backup{SiteID: site.ID, SnapshotID: "abc" + strconv.Itoa(i), Repository: "/r", Kind: "full", SizeBytes: 1 << 30})
		store.SetBackupVerified(b.ID, state.VerifyPassed, 30000)
	}
	for i := 0; i < 288; i++ {
		store.RecordMetric(state.MetricSample{CPUHeadroomPct: 80, MemHeadroomPct: 60, DiskHeadroomPct: 65})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startWatchers(ctx)

	var mu sync.Mutex
	counts := map[string]int{}
	api := s.Routes()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__count", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(counts)
		if r.URL.Query().Get("reset") != "" {
			counts = map[string]int{}
		}
	})
	mux.HandleFunc("POST /__poke", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("site"), 10, 64)
		site, err := store.MutateSite(id, func(x *state.Site) error {
			if x.Status == state.SiteActive {
				x.Status = state.SiteError
			} else {
				x.Status = state.SiteActive
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": site.Status, "at": time.Now().UnixMilli()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			mu.Lock()
			counts[r.Method+" "+r.URL.Path]++
			mu.Unlock()
		}
		api.ServeHTTP(w, r)
	})
	t.Logf("panel on http://%s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 10 * time.Minute}
	t.Fatal(srv.ListenAndServe())
}

// seedShop gives the store releases in each status the page shows, and
// cron jobs on and off.
func seedShop(t *testing.T, store *state.Store, site state.Site, now time.Time) {
	t.Helper()
	// Its database and user, as a site created through the API records them.
	if _, err := store.AddSiteDatabase(site.ID, "site_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDBUser(site.ID, "site_2", state.DBUserSecret(site.ID, "site_2"), "dev-db-password-1",
		[]state.DBGrant{{Database: "site_2", Level: "all"}}); err != nil {
		t.Fatal(err)
	}
	blocked := `{"verdict":"block","reasons":["/shop/ is 34% slower on staging, over the 15% threshold"],"measured_at":"` + now.Format(time.RFC3339) + `"}`
	for i, st := range []state.DeploymentStatus{state.DeployCreated, state.DeployPromoted, state.DeployCreated, state.DeployBlocked, state.DeployFailed} {
		d, err := store.CreateDeployment(state.Deployment{SiteID: site.ID, ReleaseID: "2026100" + strconv.Itoa(i+1) + "-0930", Status: st})
		if err != nil {
			t.Fatal(err)
		}
		switch st {
		case state.DeployPromoted:
			store.MoveDeployment(d.ID, nil, state.DeployPromoted, state.DeploymentUpdate{Note: "unverified", GuardJSON: `{"verdict":"unverified","reasons":["verification canceled"]}`})
		case state.DeployBlocked:
			store.MoveDeployment(d.ID, nil, state.DeployBlocked, state.DeploymentUpdate{GuardJSON: blocked})
		}
	}
	for _, j := range []state.CronJob{
		{SiteID: site.ID, Schedule: "*/5 * * * *", Command: "wp cron event run --due-now", Description: "WP-Cron", Enabled: true},
		{SiteID: site.ID, Schedule: "0 3 * * *", Command: "php " + site.RootPath + "/current/bin/report.php", Description: "Nightly report", Enabled: false},
	} {
		if _, err := store.CreateCronJob(j); err != nil {
			t.Fatal(err)
		}
	}
}

func strPtr(s string) *string { return &s }

// uidevRows is a page of wp_posts, n rows, one title NULL: enough rows that
// Browse lays out a page taller than the window.
func uidevRows(n int) []rpc.DBRow {
	titles := []string{"Linen shirt", "Wool scarf", "Cotton tee", "Denim jacket", "Silk tie"}
	status := []string{"publish", "draft", "trash", "publish", "private"}
	rows := make([]rpc.DBRow, 0, n)
	for i := 0; i < n; i++ {
		var title *string
		if i%7 != 3 {
			title = strPtr(titles[i%len(titles)] + " " + strconv.Itoa(i+12))
		}
		rows = append(rows, rpc.DBRow{Values: []*string{strPtr(strconv.Itoa(i + 12)), title, strPtr(status[i%len(status)]),
			strPtr(time.Date(2026, 10, 1+i%9, 9, i%60, 0, 0, time.UTC).Format(time.DateTime))}})
	}
	return rows
}
