package rpc

import (
	"errors"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Restoring a site from another server's backups (#193, ops/backup SPEC §4,
// §5, §14; internal/api SPEC §5.10).

// ManifestFormat is the version of SiteManifest a backup carries.
const ManifestFormat = 1

// MaxManifest bounds a manifest read back from a repository.
const MaxManifest = 64 << 10

// SiteManifest describes a site in each of its backups (the snapshot
// tagged part:manifest, file cloudground-site.json): what a panel needs to
// create the site again on another server before restoring it.
type SiteManifest struct {
	Format      int                `json:"format"`
	CloudGround string             `json:"cloudground"`
	Domain      string             `json:"domain"`
	Aliases     []string           `json:"aliases"`
	Type        state.SiteType     `json:"type"`
	Profile     state.Profile      `json:"profile,omitempty"`
	PHPVersion  string             `json:"php_version,omitempty"`
	Runtime     *ManifestRuntime   `json:"runtime,omitempty"`
	Cache       ManifestCache      `json:"cache"`
	Databases   []ManifestDatabase `json:"databases"`
	Root        string             `json:"root"`
}

// ManifestRuntime is a Node.js or Python site's runtime and start command.
type ManifestRuntime struct {
	Version string `json:"version,omitempty"`
	Start   string `json:"start,omitempty"`
}

// ManifestCache is the site's page and object cache settings.
type ManifestCache struct {
	Enabled     bool `json:"enabled"`
	TTLSec      int  `json:"ttl_sec,omitempty"`
	ObjectCache bool `json:"object_cache"`
}

// ManifestDatabase is one database the backup holds: its name, its engine
// (mariadb, postgres), whether it is the site's primary and whether it was
// on an external server.
type ManifestDatabase struct {
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	Primary  bool   `json:"primary,omitempty"`
	External bool   `json:"external,omitempty"`
}

// RepoSite is one site a repository holds backups of, from one server:
// its newest files snapshot, when it was taken, how many backups of it there
// are, and whether the newest carries a manifest.
type RepoSite struct {
	Domain    string    `json:"domain"`
	Hostname  string    `json:"hostname"`
	Snapshot  string    `json:"snapshot"`
	Time      time.Time `json:"time"`
	Snapshots int       `json:"snapshots"`
	Manifest  bool      `json:"manifest"`
}

// RepositorySitesResult lists every site of a repository, by domain and
// server (RepositorySites).
type RepositorySitesResult struct {
	Sites []RepoSite `json:"sites"`
}

// ForeignSnapshotParams names a files snapshot of any site and any server
// (ForeignSnapshot).
type ForeignSnapshotParams struct {
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env,omitempty"`
	SnapshotID string            `json:"snapshot_id"`
}

// Validate requires a snapshot ID.
func (p *ForeignSnapshotParams) Validate() error {
	if !snapshotID(p.SnapshotID) {
		return errors.New("invalid snapshot id")
	}
	return nil
}

// ForeignSnapshotResult is a files snapshot as a restore from another
// server needs it: restic's ID, the site it is a backup of (its site: tag),
// the server that took it, its time, its manifest (nil for a backup from
// before 1.0) and the databases its database parts hold ("" for one from
// before they were named, the site's primary).
type ForeignSnapshotResult struct {
	ID        string        `json:"id"`
	Domain    string        `json:"domain"`
	Hostname  string        `json:"hostname"`
	Time      time.Time     `json:"time"`
	Manifest  *SiteManifest `json:"manifest,omitempty"`
	Databases []string      `json:"databases"`
}

// ForeignRestore is the one exception to the rule that a restore takes only
// this server's snapshots (ops/backup SPEC §5): the panel's restore of a site
// from another server's backup (internal/api SPEC §5.10). Hostname is the
// server whose snapshot it is; Databases maps a database of the backup to
// the new site's (the primary, renamed with the site); DBPassword and
// ConnectorToken are the new site's, written into a WordPress site's
// wp-config.php over the old server's.
type ForeignRestore struct {
	Hostname       string            `json:"hostname"`
	Databases      map[string]string `json:"databases,omitempty"`
	DBPassword     string            `json:"db_password,omitempty"`
	ConnectorToken string            `json:"connector_token,omitempty"`
}

// validate holds a foreign restore to a whole restore of a named server's
// snapshot into databases with valid names.
func (f *ForeignRestore) validate(mode string, p *RestoreParams) error {
	if mode != "" && mode != "full" {
		return errors.New("a restore from another server is a full restore")
	}
	if len(p.Paths) > 0 || len(p.Databases) > 0 {
		return errors.New("a restore from another server restores everything")
	}
	h := f.Hostname
	if h == "" || len(h) > 253 || strings.ContainsAny(h, " \t\r\n\x00/") {
		return errors.New("invalid hostname of the other server")
	}
	if len(f.Databases) > 64 {
		return errors.New("too many databases")
	}
	for from, to := range f.Databases {
		if !site.ValidDBName(from) || !site.ValidDBName(to) {
			return errors.New("invalid database name")
		}
	}
	if strings.ContainsAny(f.DBPassword+f.ConnectorToken, "\x00\r\n") {
		return errors.New("invalid site credentials")
	}
	return nil
}
