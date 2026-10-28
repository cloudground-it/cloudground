package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// The site's manifest (SPEC §4, #193): a small description of the site in
// each backup, a snapshot of its own the files snapshot names, so another
// server can create the site again before restoring it.

// ManifestName is the manifest's file name inside its snapshot.
const ManifestName = "cloudground-site.json"

// Tags of the manifest: the part, and the files snapshot's link to it.
const (
	tagManifestPart = "part:manifest"
	tagManifest     = "manifest:"
)

// manifestOf describes site s with its backed-up databases.
func (o *Ops) manifestOf(ctx context.Context, s state.Site, databases []string) (rpc.SiteManifest, error) {
	m := rpc.SiteManifest{
		Format: rpc.ManifestFormat, CloudGround: version.Version, Domain: s.Domain, Aliases: append([]string{}, s.Aliases...),
		Type: s.Type, Profile: s.Profile, PHPVersion: s.PHPVersion, Root: s.RootPath,
		Cache:     rpc.ManifestCache{Enabled: s.Config.CacheEnabled, TTLSec: s.Config.CacheTTLSec, ObjectCache: s.Config.ObjectCache},
		Databases: []rpc.ManifestDatabase{},
	}
	if s.Type.IsApp() {
		m.Runtime = &rpc.ManifestRuntime{Version: s.Config.App.Version, Start: s.Config.App.Start}
	}
	for _, name := range databases {
		engine, external, err := o.DB.DatabaseKind(ctx, name)
		if err != nil {
			return rpc.SiteManifest{}, fmt.Errorf("describe the database %s: %w", name, err)
		}
		primary := s.Config.Database.Enabled && !s.Config.Database.External && name == s.Config.Database.Name
		m.Databases = append(m.Databases, rpc.ManifestDatabase{Name: name, Engine: engine, Primary: primary, External: external})
	}
	return m, nil
}

// backupManifest stores the site's manifest as a snapshot of its own, with
// the backup's tags.
func (o *Ops) backupManifest(ctx context.Context, repo host.Repo, m rpc.SiteManifest, tags []string) (host.BackupSummary, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return host.BackupSummary{}, err
	}
	args := append(append([]string{"backup", "--stdin", "--stdin-filename", ManifestName}, tags...),
		"--tag", tagManifestPart, "--json", "--quiet")
	out := host.NewResticOutput(args)
	runErr := o.runIO(ctx, repo, metaTimeout, bytes.NewReader(append(body, '\n')), out, args...)
	if runErr == nil {
		runErr = out.Err()
	}
	sum, sumErr := host.DecodeBackupSummary(out.Bytes())
	if runErr != nil {
		if sumErr == nil {
			o.forget(ctx, repo, sum.SnapshotID)
		}
		return host.BackupSummary{}, fmt.Errorf("back up the site's description: %w", runErr)
	}
	if sumErr != nil {
		return host.BackupSummary{}, fmt.Errorf("back up the site's description: %w", sumErr)
	}
	if !ValidSnapshotID(sum.SnapshotID) {
		return host.BackupSummary{}, fmt.Errorf("restic reported an invalid snapshot id")
	}
	return sum, nil
}

// manifestID is the manifest snapshot files names ("" for a backup from
// before 1.0, which has none).
func manifestID(files host.Snapshot) (string, error) {
	for _, t := range files.Tags {
		if id, ok := strings.CutPrefix(t, tagManifest); ok {
			if !ValidSnapshotID(id) {
				return "", fmt.Errorf("snapshot %s names an invalid manifest snapshot", short(files.ID))
			}
			return id, nil
		}
	}
	return "", nil
}

// readManifest reads the manifest files names, found by find (by ID or the
// ID it had before a copy, SPEC §5): a part:manifest snapshot of the same
// site and server, a JSON document of at most rpc.MaxManifest bytes naming
// that site. nil, nil for a backup without one.
func (o *Ops) readManifest(ctx context.Context, repo host.Repo, find func(context.Context, string) (host.Snapshot, error), files host.Snapshot, domain string) (*rpc.SiteManifest, error) {
	id, err := manifestID(files)
	if err != nil || id == "" {
		return nil, err
	}
	snap, err := find(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("the site's description: %w", err)
	}
	if !slices.Contains(snap.Tags, tagManifestPart) || !slices.Contains(snap.Tags, tagSite+domain) || snap.Hostname != files.Hostname || !ValidSnapshotID(snap.ID) {
		return nil, agentrpc.Invalid("snapshot %s is not the description of a backup of %s", short(id), domain)
	}
	var buf limitedBuffer
	buf.limit = rpc.MaxManifest
	if err := o.runIO(ctx, repo, metaTimeout, nil, &buf, "dump", snap.ID, "/"+ManifestName); err != nil {
		return nil, fmt.Errorf("read the site's description: %w", err)
	}
	if buf.over {
		return nil, agentrpc.Invalid("the site's description is larger than %d bytes", rpc.MaxManifest)
	}
	var m rpc.SiteManifest
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		return nil, agentrpc.Invalid("the site's description does not read: %v", err)
	}
	if m.Format < 1 || m.Domain != domain {
		return nil, agentrpc.Invalid("the site's description is not of %s", domain)
	}
	return &m, nil
}

// limitedBuffer keeps at most limit bytes and notes that more came.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)

// isPart reports whether snap is a part of a backup (a database or the
// manifest), not the files snapshot a backup is named by.
func isPart(snap host.Snapshot) bool {
	return slices.Contains(snap.Tags, tagDatabase) || slices.Contains(snap.Tags, tagManifestPart)
}

// siteOf is the domain a snapshot's site: tag names ("" for none).
func siteOf(snap host.Snapshot) string {
	for _, t := range snap.Tags {
		if d, ok := strings.CutPrefix(t, tagSite); ok {
			return d
		}
	}
	return ""
}

// RepositorySites lists every site the repository holds backups of, from
// any server: one entry per domain and server, its newest files snapshot
// (SPEC §14).
func (o *Ops) RepositorySites(ctx context.Context, p rpc.TestBackupParams) (rpc.RepositorySitesResult, error) {
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.RepositorySitesResult{}, err
	}
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return rpc.RepositorySitesResult{}, err
	}
	defer unlock()
	snaps, err := o.allSnapshots(ctx, repo)
	if err != nil {
		return rpc.RepositorySitesResult{}, err
	}
	type key struct{ domain, host string }
	groups := map[key]*rpc.RepoSite{}
	for _, s := range snaps {
		d := siteOf(s)
		if d == "" || isPart(s) || !ValidSnapshotID(s.ID) {
			continue
		}
		k := key{d, s.Hostname}
		g := groups[k]
		if g == nil {
			g = &rpc.RepoSite{Domain: d, Hostname: s.Hostname}
			groups[k] = g
		}
		g.Snapshots++
		if g.Snapshot == "" || s.Time.After(g.Time) {
			id, _ := manifestID(s)
			g.Snapshot, g.Time, g.Manifest = s.ID, s.Time, id != ""
		}
	}
	out := rpc.RepositorySitesResult{Sites: []rpc.RepoSite{}}
	for _, g := range groups {
		out.Sites = append(out.Sites, *g)
	}
	sort.Slice(out.Sites, func(i, j int) bool {
		a, b := out.Sites[i], out.Sites[j]
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		return a.Hostname < b.Hostname
	})
	return out, nil
}

// ForeignSnapshot reads one files snapshot of any site and any server, with
// its manifest and the databases its parts hold (SPEC §14): what the panel
// creates a site from. The ID is resolved as everywhere (SPEC §5).
func (o *Ops) ForeignSnapshot(ctx context.Context, p rpc.ForeignSnapshotParams) (rpc.ForeignSnapshotResult, error) {
	if !ValidSnapshotID(p.SnapshotID) {
		return rpc.ForeignSnapshotResult{}, agentrpc.Invalid("invalid snapshot id")
	}
	repo, err := o.repoOf(ctx, p.Repository, p.Password, p.Env)
	if err != nil {
		return rpc.ForeignSnapshotResult{}, err
	}
	unlock, err := o.lock(ctx, repo.URL, false)
	if err != nil {
		return rpc.ForeignSnapshotResult{}, err
	}
	defer unlock()
	snap, err := o.snapshot(ctx, repo, p.SnapshotID)
	var missing *missingSnapshot
	var all []host.Snapshot
	if errors.As(err, &missing) {
		if all, err = o.allSnapshots(ctx, repo); err != nil {
			return rpc.ForeignSnapshotResult{}, err
		}
		found, ferr := matchOriginal(all, p.SnapshotID)
		switch {
		case ferr != nil:
			return rpc.ForeignSnapshotResult{}, ferr
		case found == nil:
			return rpc.ForeignSnapshotResult{}, missing
		}
		snap, err = *found, nil
	}
	if err != nil {
		return rpc.ForeignSnapshotResult{}, err
	}
	domain := siteOf(snap)
	if domain == "" || isPart(snap) || !ValidSnapshotID(snap.ID) {
		return rpc.ForeignSnapshotResult{}, agentrpc.Invalid("snapshot %s is not the backup of a site", p.SnapshotID)
	}
	l := &siteSnaps{o: o, repo: repo, site: state.Site{Domain: domain}}
	m, err := o.readManifest(ctx, repo, l.find, snap, domain)
	if err != nil {
		return rpc.ForeignSnapshotResult{}, err
	}
	res := rpc.ForeignSnapshotResult{ID: snap.ID, Domain: domain, Hostname: snap.Hostname, Time: snap.Time, Manifest: m, Databases: []string{}}
	for _, t := range snap.Tags {
		dbID, ok := strings.CutPrefix(t, tagDB)
		if !ok || !ValidSnapshotID(dbID) {
			continue
		}
		part, err := l.find(ctx, dbID)
		if err != nil {
			return rpc.ForeignSnapshotResult{}, err
		}
		name := ""
		for _, pt := range part.Tags {
			if v, ok := strings.CutPrefix(pt, tagDBName); ok {
				name = v
			}
		}
		res.Databases = append(res.Databases, name)
	}
	return res, nil
}
