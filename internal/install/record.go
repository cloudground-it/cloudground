package install

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// InstallRecordFile is what the installer installed that was not there
// before (SPEC §9.1): `cgctl uninstall --purge-packages` takes away those
// packages and nothing else, and puts back what it removed.
const InstallRecordFile = "/var/lib/cloudground-agent/installed.json"

// InstallRecord is the record. Each list only grows, and holds a name once.
type InstallRecord struct {
	Version int `json:"version"`
	// Packages are distribution packages apt installed for CloudGround
	// (missing when the step ran); a package already present is never here.
	Packages []string `json:"packages,omitempty"`
	// PPAs are the Launchpad archives it added ("ondrej/php").
	PPAs []string `json:"ppas,omitempty"`
	// Tools are the pinned downloads it put in /usr/local/bin where no
	// file was ("/usr/local/bin/restic").
	Tools []string `json:"tools,omitempty"`
	// NginxDefaultSite is the target of the distribution's
	// sites-enabled/default link it removed, to put back.
	NginxDefaultSite string `json:"nginx_default_site,omitempty"`
	// Dirs are directories of other packages (certbot's, cron's) it
	// created because they were missing: an uninstall removes them again
	// while they are empty.
	Dirs []string `json:"dirs,omitempty"`
}

// foreignDirs are the directories the installer creates that belong to
// other packages (SPEC §3 step 10).
var foreignDirs = map[string]bool{"/var/lib/letsencrypt": true, "/var/spool/cron": true}

// recordNewDir records the topmost missing directory on the way to p, when
// p is missing.
func (e *Env) recordNewDir(p string) {
	top := ""
	for d := filepath.Clean(p); d != "/" && d != "." && !e.exists(d); d = filepath.Dir(d) {
		top = d
	}
	if top != "" {
		e.recordOrWarn(func(rec *InstallRecord) { rec.Dirs = addOnce(rec.Dirs, top) })
	}
}

// ReadInstallRecord reads the record; a server installed before it existed
// has none (the zero record: nothing to purge).
func (e *Env) ReadInstallRecord() (InstallRecord, error) {
	var r InstallRecord
	b, err := e.readFile(InstallRecordFile)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}

// record adds to the record. The directory is the agent's state directory,
// created root-only when the step that records runs before `dirs`.
func (e *Env) record(update func(*InstallRecord)) error {
	r, err := e.ReadInstallRecord()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(r)
	update(&r)
	r.Version = 1
	after, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if string(before) == string(after) {
		return nil
	}
	dir := e.path(filepath.Dir(InstallRecordFile))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return e.writeFile(InstallRecordFile, append(after, '\n'), want{mode: 0o600, user: "root", group: "root"})
}

// recordOrWarn is record whose failure is a line in the log: the install
// goes on, and an uninstall then purges less, never more.
func (e *Env) recordOrWarn(update func(*InstallRecord)) {
	if err := e.record(update); err != nil {
		e.logf("warning: could not update %s: %v", InstallRecordFile, err)
	}
}

// addOnce appends the names not in list yet.
func addOnce(list []string, names ...string) []string {
	for _, n := range names {
		if n != "" && !slices.Contains(list, n) {
			list = append(list, n)
		}
	}
	return list
}
