package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
)

// While a migration switches a WordPress site's database and code, the site
// is in WordPress's maintenance mode: a .maintenance file at the root of the
// live release (ABSPATH) setting $upgrading makes wp-settings answer 503 to
// every request before it touches the database (SPEC §4.4). $upgrading is
// time() itself, so the mode does not lapse after WordPress's ten minutes;
// removing the file ends it.
const maintenanceName = ".maintenance"

// maintenanceBody is the file a migration writes, and the only one it
// removes: a .maintenance the site put there itself is left alone.
var maintenanceBody = []byte("<?php $upgrading = time(); // CloudGround: a migration is switching this site\n")

// maintenanceJournal is the root-only directory, under WorkDir, recording
// the maintenance files in place, so an agent that died mid-migration
// removes them at its next start (Sweep).
const maintenanceJournal = "migration-maintenance"

// journalEntry is one site in maintenance.
type journalEntry struct {
	Root  string   `json:"root"`
	Files []string `json:"files"`
}

func (o *Ops) journalPath(siteID int64) string {
	return filepath.Join(o.WorkDir, maintenanceJournal, strconv.FormatInt(siteID, 10)+".json")
}

// maintenanceOn writes the maintenance file into each release, as the site
// user and through the site's root (never through a link), after recording
// them in the journal. A release already holding a .maintenance keeps it.
func (m *run) maintenanceOn() error {
	var files []string
	for _, id := range []string{m.oldID, m.p.ReleaseID} {
		files = append(files, path.Join(release.Dir, id, maintenanceName))
	}
	j, err := json.Marshal(journalEntry{Root: m.p.Site.RootPath, Files: files})
	if err != nil {
		return err
	}
	jp := m.o.journalPath(m.p.Site.ID)
	if err := os.MkdirAll(filepath.Dir(jp), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(jp, j, 0o600); err != nil {
		return err
	}
	m.maintenance = files
	for _, rel := range files {
		f, err := m.r.Create(rel, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("put the site in maintenance: %w", err)
		}
		_, err = f.Write(maintenanceBody)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("put the site in maintenance: %w", err)
		}
	}
	return nil
}

// maintenanceOff removes the maintenance files and the journal entry. It
// can run any number of times.
func (m *run) maintenanceOff() {
	if m.maintenance == nil {
		return
	}
	if err := removeMaintenance(m.r, m.maintenance); err != nil {
		m.o.log().Warn("taking the site out of maintenance failed; the agent retries at its next start", "site", m.p.Site.Domain, "err", err)
		return
	}
	jp := m.o.journalPath(m.p.Site.ID)
	_ = os.Remove(jp)
	_ = os.Remove(filepath.Dir(jp)) // only when no other site is in maintenance
	m.maintenance = nil
}

// removeMaintenance removes each file that is a regular file holding
// exactly maintenanceBody; a missing one is fine.
func removeMaintenance(r *sitefs.Root, files []string) error {
	var errs []error
	for _, rel := range files {
		b, err := r.ReadFile(rel, int64(len(maintenanceBody))+1)
		if err != nil || !bytes.Equal(b, maintenanceBody) {
			continue
		}
		if err := r.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Sweep takes out of maintenance every site a migration left in it: the
// agent died before it could. Run it at start, before serving.
func (o *Ops) Sweep() error {
	dir := filepath.Join(o.WorkDir, maintenanceJournal)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		jp := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(jp)
		var j journalEntry
		if err == nil {
			err = json.Unmarshal(b, &j)
		}
		if err == nil {
			var r *sitefs.Root
			if r, err = sitefs.Open(j.Root, sitefs.Owner{}); err == nil {
				err = removeMaintenance(r, j.Files)
				r.Close()
			}
		}
		if errors.Is(err, fs.ErrNotExist) {
			err = nil // the site is gone
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", jp, err))
			continue
		}
		o.log().Warn("a migration was cut short; its site is out of maintenance again", "root", j.Root)
		_ = os.Remove(jp)
	}
	return errors.Join(errs...)
}
