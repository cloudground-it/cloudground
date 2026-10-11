package api

// Which recorded managed files are a site's (SPEC.md §3.9). The drift scan
// compares these records with the disk, so a record left behind by a site
// that no longer writes the file shows as drift for ever.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// siteOwnsPath reports whether the managed file at path is site's. Names
// are matched whole: "cg-site-1" is not "cg-site-10", and "example.com"
// is not "dev.example.com".
func siteOwnsPath(site state.Site, path string) bool {
	name, dir := filepath.Base(path), filepath.Base(filepath.Dir(path))
	// The site's blocking rules (#25).
	if name == "cloudground-block-"+strconv.FormatInt(site.ID, 10)+".conf" {
		return true
	}
	// A protected path's password file (#26).
	if strings.HasPrefix(name, site.Domain+".path-") && strings.HasSuffix(name, ".htpasswd") {
		return true
	}
	owned := map[string]bool{
		site.Domain + ".conf":     true,
		site.Domain + ".htpasswd": true,
		"cloudground-cache-" + strconv.FormatInt(site.ID, 10) + ".conf": true,
	}
	if u := site.SystemUser; u != "" {
		unit := "cloudground-php-" + u + ".service"
		owned[u+".conf"] = true
		owned[unit] = true
		owned[u+".slice"] = true // the site's slice unit, its limits
		// The PHP master's drop-ins (its slice) live in <unit>.d/.
		if dir == unit+".d" {
			return true
		}
	}
	if owned[name] {
		return true
	}
	return site.RootPath != "" && strings.HasPrefix(path, site.RootPath+"/")
}

// siteFiles is the recorded managed files of site, with their hashes.
func (s *Server) siteFiles(site state.Site) map[string]string {
	all, err := s.Store.ManagedFiles()
	if err != nil {
		return nil
	}
	mine := map[string]string{}
	for path, sum := range all {
		if siteOwnsPath(site, path) {
			mine[path] = sum
		}
	}
	return mine
}

// forgetSiteFiles stops tracking every file of a site that is gone.
func (s *Server) forgetSiteFiles(site state.Site) {
	for path := range s.siteFiles(site) {
		s.logIfErr(s.Store.RemoveManagedFile(path), "RemoveManagedFile")
	}
}

// recordRendered records what a full render of site wrote and stops
// tracking the site's configuration files it did not write (the shared pool
// file a site had before it got its own PHP master, say). Files in the
// site's tree (the connector) are a deploy's, not a render's, and stay.
func (s *Server) recordRendered(site state.Site, files []rpc.ManagedFile) {
	wrote := make(map[string]bool, len(files))
	for _, f := range files {
		wrote[f.Path] = true
		_ = s.Store.RecordManagedFile(f.Path, f.SHA256)
	}
	inTree := func(p string) bool { return site.RootPath != "" && strings.HasPrefix(p, site.RootPath+"/") }
	for path := range s.siteFiles(site) {
		if !wrote[path] && !inTree(path) {
			_ = s.Store.RemoveManagedFile(path)
		}
	}
}

// pruneOrphanManagedFiles drops the records of per-site files that belong
// to none of sites (a deleted site's PHP unit or staging password file,
// reported as drift otherwise). It reports whether the pass was complete:
// the records were read and the agent said where sites live, so files
// under that root were judged too; otherwise the caller tries again.
func (s *Server) pruneOrphanManagedFiles(sites []state.Site) bool {
	all, err := s.Store.ManagedFiles()
	if err != nil {
		return false
	}
	// capabilities falls back to its last answer; without one, the other
	// marks of a per-site file still apply.
	root := ""
	if caps, err := s.capabilities(context.Background()); err == nil && caps.Paths.SitesRoot != "" {
		root = filepath.Clean(caps.Paths.SitesRoot) + "/"
	}
	perSite := func(p string) bool {
		name := filepath.Base(p)
		return strings.Contains(name, "cg-site-") ||
			strings.Contains(filepath.Base(filepath.Dir(p)), "cg-site-") ||
			strings.HasSuffix(name, ".htpasswd") ||
			(root != "" && strings.HasPrefix(p, root))
	}
	for path := range all {
		if !perSite(path) {
			continue
		}
		claimed := false
		for _, site := range sites {
			if siteOwnsPath(site, path) {
				claimed = true
				break
			}
		}
		if !claimed {
			_ = s.Store.RemoveManagedFile(path)
		}
	}
	return root != ""
}
