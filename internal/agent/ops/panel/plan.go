package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/version"
)

// Plan is what the agent hands its update guard: what was swapped, and how
// to take it back. It is written root-only in the agent's work directory.
type Plan struct {
	Version  string `json:"version"`
	Previous string `json:"previous"`
	BinDir   string `json:"bin_dir"`
	// Backed are binaries whose previous version is <name>.bak; Fresh did
	// not exist before and are removed on rollback.
	Backed []string `json:"backed"`
	Fresh  []string `json:"fresh,omitempty"`
	// Units are unit files replaced, their previous content in UnitBackup;
	// FreshUnits did not exist before.
	SystemdDir    string   `json:"systemd_dir"`
	Units         []string `json:"units,omitempty"`
	FreshUnits    []string `json:"fresh_units,omitempty"`
	UnitBackup    string   `json:"unit_backup"`
	SocketChanged bool     `json:"socket_changed,omitempty"`
	APISocket     string   `json:"api_socket"`
	StateDir      string   `json:"state_dir"`
	// The pre-update snapshot (SPEC §2.5): the API's state database and
	// the agent's records, copied under SnapshotRoot before the release
	// first starts, the newest KeepSnapshots kept.
	StateDB       string   `json:"state_db,omitempty"`
	Records       []string `json:"records,omitempty"`
	SnapshotRoot  string   `json:"snapshot_root,omitempty"`
	KeepSnapshots int      `json:"keep_snapshots,omitempty"`
}

// planName is the plan's file name in the work directory.
const planName = "update-plan.json"

// Result is the guard's verdict, kept in <StateDir>/update-result.json.
type Result struct {
	Version  string `json:"version"`
	Previous string `json:"previous"`
	OK       bool   `json:"ok"`
	// RolledBack: the release did not come up and the previous one was
	// put back; RestoredHealthy says whether that one came up.
	RolledBack      bool   `json:"rolled_back,omitempty"`
	RestoredHealthy bool   `json:"restored_healthy,omitempty"`
	Error           string `json:"error,omitempty"`
	// Snapshot is the directory of the pre-update snapshot; StateRestored
	// says a rollback put it back.
	Snapshot      string `json:"snapshot,omitempty"`
	StateRestored bool   `json:"state_restored,omitempty"`
	// Started is when the guard began, so a waiting `cgctl update` can
	// tell this result from an older one.
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished"`
}

// ResultName is the guard's verdict file in the state directory.
const ResultName = "update-result.json"

// highestName is the newest release installed here, in the state
// directory. The installer reads it as well.
const highestName = "highest-release"

// ReadPlan reads a plan the agent wrote. It refuses a symlink and, when run
// as root, a file anyone but root could have written.
func ReadPlan(path string) (Plan, error) {
	var p Plan
	f, err := os.OpenFile(path, os.O_RDONLY|nofollow, 0)
	if err != nil {
		return p, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return p, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return p, fmt.Errorf("%s: not a private regular file", path)
	}
	if os.Geteuid() == 0 && ownerUID(fi) != 0 {
		return p, fmt.Errorf("%s: not owned by root", path)
	}
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, p.check()
}

// check refuses a plan whose names could leave its directories.
func (p Plan) check() error {
	for _, d := range []string{p.BinDir, p.SystemdDir, p.UnitBackup, p.StateDir, p.APISocket} {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("plan: %q is not absolute", d)
		}
	}
	for _, d := range append([]string{p.StateDB, p.SnapshotRoot}, p.Records...) {
		if d != "" && !filepath.IsAbs(d) {
			return fmt.Errorf("plan: %q is not absolute", d)
		}
	}
	for _, list := range [][]string{p.Backed, p.Fresh, p.Units, p.FreshUnits} {
		for _, n := range list {
			if n == "" || strings.ContainsAny(n, "/\x00") || n == "." || n == ".." {
				return fmt.Errorf("plan: bad name %q", n)
			}
		}
	}
	return nil
}

func (p Plan) write(path string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return rootfile.Replace(path, append(b, '\n'), 0o600)
}

// restoreFiles puts the previous binaries and unit files back. Each step is
// tried even when an earlier one failed.
func (p Plan) restoreFiles() error {
	var errs []error
	for _, b := range p.Backed {
		live := filepath.Join(p.BinDir, b)
		if err := os.Rename(live+".bak", live); err != nil {
			errs = append(errs, err)
		}
	}
	for _, b := range p.Fresh {
		if err := os.Remove(filepath.Join(p.BinDir, b)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, u := range p.Units {
		data, err := os.ReadFile(filepath.Join(p.UnitBackup, u))
		if err == nil {
			err = rootfile.Replace(filepath.Join(p.SystemdDir, u), data, 0o644)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, u := range p.FreshUnits {
		if err := os.Remove(filepath.Join(p.SystemdDir, u)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// unitsChanged reports whether any unit file was replaced or added.
func (p Plan) unitsChanged() bool { return len(p.Units)+len(p.FreshUnits) > 0 }

// ReadResult reads the last update's verdict from the state directory.
func ReadResult(stateDir string) (Result, error) {
	var r Result
	b, err := rootfile.ReadNoFollow(filepath.Join(stateDir, ResultName))
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}

// HighestInstalled is the newest release this server has run, from
// <stateDir>/highest-release, or "" when none is recorded.
func HighestInstalled(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, highestName))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if _, ok := version.Parse(v); !ok {
		return ""
	}
	return v
}

// recordHighest raises the recorded newest release to v.
func recordHighest(stateDir, v string) error {
	if cur := HighestInstalled(stateDir); cur != "" && !version.Newer(v, cur) {
		return nil
	}
	return rootfile.Replace(filepath.Join(stateDir, highestName), []byte(v+"\n"), 0o600)
}
