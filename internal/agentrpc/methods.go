package agentrpc

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Lock is the per-site lock the server takes before a handler runs
// (SPEC.md §3.6), keyed on the first site of the parameters.
type Lock int

const (
	// LockNone: the handler locks for itself, or needs no lock.
	LockNone Lock = iota
	// LockSite takes the site's lock, for a site that may be half gone.
	LockSite
	// LockLiveSite takes the site's lock and requires the site to exist.
	LockLiveSite
)

// Spec is one row of the method table (SPEC.md §5).
type Spec struct {
	Name     string
	Timeout  time.Duration
	Sites    bool
	Lock     Lock
	ReadOnly bool
	Fail     string
	// Params and Result are the method's wire types, set by the table
	// (the golden wire tests of internal/rpc walk them).
	Params, Result reflect.Type
}

// Method is a typed handle on a row of the table: P and R are the parameter
// and result types, so a client call or a handler with the wrong types does
// not compile.
type Method[P, R any] struct{ spec *Spec }

// Name is the method's wire name.
func (m Method[P, R]) Name() string { return m.spec.Name }

// Spec returns the method's row.
func (m Method[P, R]) Spec() Spec { return *m.spec }

// None is the parameter or result of a method that has none.
type None = struct{}

// SiteCarrier is implemented (on the pointer) by every parameter type that
// carries sites. The server validates each non-nil one and writes the
// validated value back.
type SiteCarrier interface {
	Sites() []*state.Site
}

// Validator is implemented (on the pointer) by a parameter type with rules
// of its own (internal/rpc SPEC.md §3). The server checks them after the
// sites and before the lock; the error's text is the user-safe message of
// an invalid error.
type Validator interface {
	Validate() error
}

var table = map[string]*Spec{}

func define[P, R any](s Spec) Method[P, R] {
	if s.Name == "" || s.Timeout <= 0 || s.Fail == "" {
		panic(fmt.Sprintf("agentrpc: incomplete method %+v", s))
	}
	if _, dup := table[s.Name]; dup {
		panic("agentrpc: method defined twice: " + s.Name)
	}
	_, carries := any(new(P)).(SiteCarrier)
	if carries != s.Sites {
		panic(fmt.Sprintf("agentrpc: %s: params carry sites = %v but the table says Sites = %v", s.Name, carries, s.Sites))
	}
	if s.Lock != LockNone && !s.Sites {
		panic("agentrpc: " + s.Name + " locks a site but takes none")
	}
	s.Params, s.Result = reflect.TypeFor[P](), reflect.TypeFor[R]()
	table[s.Name] = &s
	return Method[P, R]{spec: table[s.Name]}
}

// Lookup returns the row for a wire name.
func Lookup(name string) (Spec, bool) {
	s, ok := table[name]
	if !ok {
		return Spec{}, false
	}
	return *s, true
}

// Specs lists the whole table, sorted by name.
func Specs() []Spec {
	out := make([]Spec, 0, len(table))
	for _, s := range table {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

const (
	sec  = time.Second
	mins = time.Minute
	hrs  = time.Hour
)

// The method table. Timeouts are the server's deadline for the handler;
// each is above the longest internal timeout of the tools the handler runs
// (restic 6 h, prune 3 h, wp-cli and the rest 10 min per command), and none
// is below the 30 minutes every call had before unless the method is a
// bounded read or a single quick change.
var (
	Ping            = define[None, map[string]string](Spec{Name: "Ping", Timeout: 10 * sec, ReadOnly: true, Fail: "The agent did not answer"})
	Capabilities    = define[None, rpc.Capabilities](Spec{Name: "Capabilities", Timeout: 10 * sec, ReadOnly: true, Fail: "Could not read what this server offers"})
	SystemStatus    = define[None, rpc.SystemStatusResult](Spec{Name: "SystemStatus", Timeout: 30 * sec, ReadOnly: true, Fail: "Could not read the system status"})
	SiteUsage       = define[rpc.SiteUsageParams, rpc.SiteUsageResult](Spec{Name: "SiteUsage", Timeout: 2 * mins, ReadOnly: true, Fail: "Could not read the sites' resource usage"})
	CreateSite      = define[rpc.CreateSiteParams, rpc.CreateSiteResult](Spec{Name: "CreateSite", Timeout: 45 * mins, Sites: true, Lock: LockSite, Fail: "Could not create the site"})
	DeleteSite      = define[rpc.SiteRef, map[string]any](Spec{Name: "DeleteSite", Timeout: 15 * mins, Sites: true, Lock: LockSite, Fail: "Could not delete the site"})
	ApplySiteConfig = define[rpc.SiteRef, rpc.ApplyResult](Spec{Name: "ApplySiteConfig", Timeout: 15 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not apply the site's configuration"})
	SiteRouting     = define[rpc.SiteRef, rpc.RoutingView](Spec{Name: "SiteRouting", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the site's routing"})
	SetSiteRouting  = define[rpc.RoutingParams, rpc.RoutingResult](Spec{Name: "SetSiteRouting", Timeout: 5 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not change the site's routing"})
	CheckDocRoot    = define[rpc.SiteRef, None](Spec{Name: "CheckDocRoot", Timeout: 30 * sec, Sites: true, ReadOnly: true, Fail: "Could not check the site's public root"})
	IssueCert       = define[rpc.CertificateParams, rpc.ApplyResult](Spec{Name: "IssueCertificate", Timeout: 20 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not issue the certificate"})
	CertStatus      = define[rpc.CertStatusParams, rpc.CertStatusResult](Spec{Name: "CertStatus", Timeout: 1 * mins, ReadOnly: true, Fail: "Could not read the certificate status"})
	CustomCert      = define[rpc.CustomCertParams, rpc.ApplyResult](Spec{Name: "CustomCertificate", Timeout: 5 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not install the certificate"})
	CreateDatabase  = define[rpc.SiteDatabaseParams, map[string]string](Spec{Name: "CreateDatabase", Timeout: 5 * mins, Sites: true, Fail: "Could not create the database"})
	DropDatabase    = define[rpc.SiteDatabaseParams, map[string]string](Spec{Name: "DropDatabase", Timeout: 30 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not drop the database"})
	CreateDBUser    = define[rpc.DBUserParams, map[string]string](Spec{Name: "CreateDatabaseUser", Timeout: 2 * mins, Sites: true, Fail: "Could not create the database user"})
	DropDBUser      = define[rpc.DBUserParams, map[string]string](Spec{Name: "DropDatabaseUser", Timeout: 2 * mins, Sites: true, Fail: "Could not delete the database user"})
	SetDBPassword   = define[rpc.DBUserParams, map[string]string](Spec{Name: "SetDatabaseUserPassword", Timeout: 15 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not change the database user's password"})
	SetDBGrants     = define[rpc.DBUserParams, map[string]string](Spec{Name: "SetDatabaseUserGrants", Timeout: 2 * mins, Sites: true, Fail: "Could not change the database user's privileges"})
	ListDatabases   = define[rpc.DBListParams, rpc.DBListResult](Spec{Name: "ListDatabases", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the site's databases"})
	DeployRelease   = define[rpc.DeployParams, rpc.DeployResult](Spec{Name: "DeployRelease", Timeout: 45 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not build the release"})
	PromoteRelease  = define[rpc.ReleaseParams, map[string]string](Spec{Name: "PromoteRelease", Timeout: 10 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not promote the release"})
	RollbackRelease = define[rpc.ReleaseParams, map[string]string](Spec{Name: "RollbackRelease", Timeout: 10 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not roll back the release"})
	CurrentRelease  = define[rpc.CurrentReleaseParams, rpc.CurrentReleaseResult](Spec{Name: "CurrentRelease", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the live release"})
	RemoveReleases  = define[rpc.RemoveReleasesParams, rpc.RemoveReleasesResult](Spec{Name: "RemoveReleases", Timeout: 10 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not remove the releases"})
	PruneRepository = define[rpc.TestBackupParams, map[string]any](Spec{Name: "PruneRepository", Timeout: 4 * hrs, Fail: "Could not prune the backup repository"})
	CreateStaging   = define[rpc.StagingParams, rpc.CreateSiteResult](Spec{Name: "CreateStaging", Timeout: 2 * hrs, Sites: true, Fail: "Could not create the staging site"})
	StagingDiff     = define[rpc.StagingParams, rpc.StagingDiffResult](Spec{Name: "StagingDiff", Timeout: 15 * mins, Sites: true, ReadOnly: true, Fail: "Could not compare staging with production"})
	StagingTables   = define[rpc.StagingParams, rpc.StagingTablesResult](Spec{Name: "StagingTables", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the staging site's tables"})
	SyncStagingDB   = define[rpc.SyncStagingDBParams, map[string]any](Spec{Name: "SyncStagingDatabase", Timeout: 2 * hrs, Sites: true, Fail: "Could not push the database tables"})
	RunBackup       = define[rpc.BackupParams, rpc.BackupResult](Spec{Name: "RunBackup", Timeout: 7 * hrs, Sites: true, Fail: "The backup failed"})
	RestoreSnapshot = define[rpc.RestoreParams, map[string]string](Spec{Name: "RestoreSnapshot", Timeout: 7 * hrs, Sites: true, Lock: LockLiveSite, Fail: "The restore failed"})
	VerifyBackup    = define[rpc.RestoreParams, rpc.VerifyResult](Spec{Name: "VerifyBackup", Timeout: 7 * hrs, Sites: true, Fail: "The restore test failed"})
	PurgeCache      = define[rpc.PurgeParams, rpc.PurgeResult](Spec{Name: "PurgeCache", Timeout: 5 * mins, Sites: true, Fail: "Could not purge the cache"})
	ReloadWebServer = define[None, None](Spec{Name: "ReloadWebServer", Timeout: 2 * mins, Fail: "Could not reload the web server"})
	CheckDrift      = define[rpc.DriftParams, rpc.DriftResult](Spec{Name: "CheckDrift", Timeout: 5 * mins, ReadOnly: true, Fail: "Could not check the configuration for drift"})
	RestartService  = define[rpc.ServiceParams, map[string]string](Spec{Name: "RestartService", Timeout: 5 * mins, Fail: "Could not restart the service"})
	ServiceStatus   = define[None, rpc.ServiceStatusResult](Spec{Name: "ServiceStatus", Timeout: 1 * mins, ReadOnly: true, Fail: "Could not read the services' status"})
	TailLog         = define[rpc.TailLogParams, rpc.TailLogResult](Spec{Name: "TailLog", Timeout: 1 * mins, ReadOnly: true, Fail: "Could not read the log"})
	ApplyCron       = define[rpc.CronParams, rpc.CronResult](Spec{Name: "ApplyCron", Timeout: 2 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not install the site's cron jobs"})
	RunCron         = define[rpc.RunCronParams, rpc.RunCronResult](Spec{Name: "RunCron", Timeout: 7 * mins, Sites: true, Fail: "Could not run the cron job"})
	CronRuns        = define[rpc.CronRunsParams, rpc.CronRunsResult](Spec{Name: "CronRuns", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the cron jobs' last runs"})
	FirewallStatus  = define[None, rpc.FirewallStatusResult](Spec{Name: "FirewallStatus", Timeout: 1 * mins, ReadOnly: true, Fail: "Could not read the firewall status"})
	PHPExtensions   = define[rpc.PHPExtensionsParams, rpc.PHPExtensionsResult](Spec{Name: "PHPExtensions", Timeout: 1 * mins, ReadOnly: true, Fail: "Could not list the PHP extensions"})
	PHPExtInstall   = define[rpc.PHPExtensionInstallParams, rpc.PHPExtensionsResult](Spec{Name: "PHPExtensionInstall", Timeout: 15 * mins, Fail: "Could not install the PHP extension"})
	FirewallRule    = define[rpc.FirewallRuleParams, map[string]string](Spec{Name: "FirewallRule", Timeout: 2 * mins, Fail: "Could not change the firewall"})
	FirewallEnable  = define[rpc.FirewallEnableParams, rpc.FirewallStatusResult](Spec{Name: "FirewallEnable", Timeout: 2 * mins, Fail: "Could not turn the firewall on"})
	DBQuery         = define[rpc.DBQueryParams, rpc.DBQueryResult](Spec{Name: "DBQuery", Timeout: 3 * mins, Sites: true, Fail: "The query failed"})
	DBExport        = define[rpc.DBExportParams, rpc.DBExportResult](Spec{Name: "DBExport", Timeout: 2 * hrs, Sites: true, Fail: "Could not export the database"})
	DBTables        = define[rpc.DBTablesParams, rpc.DBTablesResult](Spec{Name: "DBTables", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the database's tables"})
	DBImport        = define[rpc.DBImportParams, rpc.DBImportResult](Spec{Name: "DBImport", Timeout: 3 * hrs, Sites: true, Lock: LockLiveSite, Fail: "Could not import the database"})
	ListFiles       = define[rpc.ListFilesParams, rpc.ListFilesResult](Spec{Name: "ListFiles", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the files"})
	ReadFile        = define[rpc.ReadFileParams, rpc.ReadFileResult](Spec{Name: "ReadFile", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the file"})
	WriteFile       = define[rpc.WriteFileParams, map[string]string](Spec{Name: "WriteFile", Timeout: 2 * mins, Sites: true, Fail: "Could not write the file"})
	TransferFile    = define[rpc.TransferFileParams, rpc.TransferFileResult](Spec{Name: "TransferFile", Timeout: 5 * mins, Sites: true, Fail: "Could not transfer the file"})
	ManageFile      = define[rpc.ManageFileParams, map[string]string](Spec{Name: "ManageFile", Timeout: 15 * mins, Sites: true, Fail: "Could not change the file"})
	SetSFTP         = define[rpc.SFTPParams, map[string]string](Spec{Name: "SetSFTP", Timeout: 2 * mins, Sites: true, Fail: "Could not change SFTP access"})
	SSHKeys         = define[rpc.SSHKeyParams, rpc.SSHKeysResult](Spec{Name: "SSHKeys", Timeout: 1 * mins, Sites: true, Fail: "Could not change the SSH keys"})
	SetShell        = define[rpc.ShellParams, map[string]string](Spec{Name: "SetShell", Timeout: 3 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not change the SSH shell"})
	WPMagicLogin    = define[rpc.WPParams, rpc.WPLoginResult](Spec{Name: "WPMagicLogin", Timeout: 2 * mins, Sites: true, Fail: "Could not create a login link"})
	WPPlugins       = define[rpc.WPParams, rpc.WPPluginsResult](Spec{Name: "WPPlugins", Timeout: 5 * mins, Sites: true, ReadOnly: true, Fail: "Could not list plugins and themes"})
	WPUpdate        = define[rpc.WPParams, map[string]string](Spec{Name: "WPUpdate", Timeout: 45 * mins, Sites: true, Lock: LockLiveSite, Fail: "The update failed"})
	WPObjectCache   = define[rpc.WPParams, map[string]string](Spec{Name: "WPObjectCache", Timeout: 15 * mins, Sites: true, Lock: LockLiveSite, Fail: "Could not change the object cache"})
	PanelCert       = define[rpc.PanelCertParams, map[string]string](Spec{Name: "PanelCertificate", Timeout: 20 * mins, Fail: "Could not issue the panel certificate"})
	SelfUpdate      = define[rpc.SelfUpdateParams, rpc.SelfUpdateResult](Spec{Name: "SelfUpdate", Timeout: 20 * mins, Fail: "The update failed"})
	CacheStats      = define[rpc.WPParams, rpc.CacheStatsResult](Spec{Name: "CacheStats", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the cache statistics"})
	WarmCache       = define[rpc.WarmParams, rpc.WarmResult](Spec{Name: "WarmCache", Timeout: 30 * mins, Sites: true, Fail: "Could not warm the cache"})
	TestBackup      = define[rpc.TestBackupParams, map[string]any](Spec{Name: "TestBackup", Timeout: 15 * mins, Fail: "The backup repository is not reachable"})
	BackupRetention = define[rpc.RetentionParams, rpc.RetentionResult](Spec{Name: "BackupRetention", Timeout: 4 * hrs, Sites: true, Fail: "Could not apply the backup retention"})
	ForgetSnapshot  = define[rpc.ForgetSnapshotParams, rpc.ForgetSnapshotResult](Spec{Name: "ForgetSnapshot", Timeout: 4 * hrs, Sites: true, Fail: "Could not delete the backup"})
	ImportMigration = define[rpc.MigrationParams, rpc.MigrationResult](Spec{Name: "ImportMigration", Timeout: 7 * hrs, Sites: true, Lock: LockLiveSite, Fail: "The migration failed"})
)

// Quarry (internal/agent/ops/dbstudio/SPEC.md §1). The write methods
// carry the caller's ReadOnly; the agent then works through a read-only
// account.
var (
	DBStudioTables          = define[rpc.DBStudioTablesParams, rpc.DBStudioTablesResult](Spec{Name: "DBStudioTables", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the database's tables"})
	DBStudioStructure       = define[rpc.DBStudioTableParams, rpc.DBStudioStructure](Spec{Name: "DBStudioStructure", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the table's structure"})
	DBStudioRows            = define[rpc.DBStudioRowsParams, rpc.DBStudioRowsResult](Spec{Name: "DBStudioRows", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the table's rows"})
	DBStudioEdit            = define[rpc.DBStudioEditParams, rpc.DBStudioEditResult](Spec{Name: "DBStudioEdit", Timeout: 3 * mins, Sites: true, Fail: "Could not change the rows"})
	DBStudioAlter           = define[rpc.DBStudioAlterParams, rpc.DBStudioAlterResult](Spec{Name: "DBStudioAlter", Timeout: 15 * mins, Sites: true, Fail: "Could not change the table"})
	DBStudioExport          = define[rpc.DBStudioExportParams, rpc.DBStudioExportResult](Spec{Name: "DBStudioExport", Timeout: 3 * hrs, Sites: true, Fail: "Could not export the database"})
	DBStudioFiles           = define[rpc.DBStudioSiteParams, rpc.DBStudioFilesResult](Spec{Name: "DBStudioFiles", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the database files"})
	DBStudioReadChunk       = define[rpc.DBStudioChunkParams, rpc.DBStudioChunkResult](Spec{Name: "DBStudioReadChunk", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the export"})
	DBStudioUploadChunk     = define[rpc.DBStudioUploadParams, rpc.DBStudioUploadResult](Spec{Name: "DBStudioUploadChunk", Timeout: 2 * mins, Sites: true, Fail: "Could not store the upload"})
	DBStudioDeleteFile      = define[rpc.DBStudioDeleteParams, map[string]string](Spec{Name: "DBStudioDeleteFile", Timeout: 1 * mins, Sites: true, Fail: "Could not delete the file"})
	DBStudioImportCSV       = define[rpc.DBStudioImportCSVParams, rpc.DBStudioImportCSVResult](Spec{Name: "DBStudioImportCSV", Timeout: 3 * hrs, Sites: true, Lock: LockLiveSite, Fail: "Could not import the CSV file"})
	DBStudioWPAutoload      = define[rpc.DBStudioSiteParams, rpc.DBStudioAutoloadResult](Spec{Name: "DBStudioWPAutoload", Timeout: 2 * mins, Sites: true, ReadOnly: true, Fail: "Could not read the autoloaded options"})
	DBStudioWPSearchReplace = define[rpc.DBStudioSearchReplaceParams, rpc.DBStudioSearchReplaceResult](Spec{Name: "DBStudioWPSearchReplace", Timeout: 45 * mins, Sites: true, Lock: LockLiveSite, Fail: "The search and replace failed"})
	DBStudioWPTransients    = define[rpc.DBStudioTransientsParams, rpc.DBStudioTransientsResult](Spec{Name: "DBStudioWPTransients", Timeout: 15 * mins, Sites: true, Fail: "Could not delete the expired transients"})
	DBStudioQuery           = define[rpc.DBStudioQueryParams, rpc.DBQueryResult](Spec{Name: "DBStudioQuery", Timeout: 3 * mins, Sites: true, Fail: "The query failed"})
	DBStudioSimulate        = define[rpc.DBStudioSimulateParams, rpc.DBStudioSimulateResult](Spec{Name: "DBStudioSimulate", Timeout: 3 * mins, Sites: true, Fail: "The simulation failed"})
	DBStudioSearch          = define[rpc.DBStudioSearchParams, rpc.DBStudioSearchResult](Spec{Name: "DBStudioSearch", Timeout: 3 * mins, Sites: true, ReadOnly: true, Fail: "The search failed"})
	// A database user signed in to Quarry with its own password
	// (Fase 9b, ops/dbstudio SPEC §11).
	DBStudioOpen      = define[rpc.DBStudioOpenParams, rpc.DBStudioOpenResult](Spec{Name: "DBStudioOpen", Timeout: 1 * mins, Sites: true, Fail: "Could not sign in to the database"})
	DBStudioClose     = define[rpc.DBStudioLoginParams, None](Spec{Name: "DBStudioClose", Timeout: 1 * mins, Sites: true, Fail: "Could not end the Quarry session"})
	DBStudioDatabases = define[rpc.DBStudioLoginParams, rpc.DBStudioDatabasesResult](Spec{Name: "DBStudioDatabases", Timeout: 1 * mins, Sites: true, ReadOnly: true, Fail: "Could not list the databases"})
	DBStudioImportSQL = define[rpc.DBStudioImportSQLParams, rpc.DBImportResult](Spec{Name: "DBStudioImportSQL", Timeout: 3 * hrs, Sites: true, Lock: LockLiveSite, Fail: "Could not import the database"})
)
