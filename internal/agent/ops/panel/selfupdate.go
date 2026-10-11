package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/version"
)

// Binaries are the release's programs, all required in a manifest.
var Binaries = []string{"panel-api", "cgctl", "panel-agent"}

// OptionalBinaries are installed when the manifest lists them (a release
// made before one existed does not): cg-site-shell, the gate client, which
// panel-agent stands in for until it is there (R-10.5).
var OptionalBinaries = []string{"cg-site-shell"}

// Units are the release's unit files, installed when the manifest lists
// them.
var Units = []string{AgentUnit, APIUnit, APISocketUnit}

// artifact is one file of a release on its way in.
type artifact struct {
	name   string
	live   string
	staged string
	unit   bool
}

// SelfUpdate installs the newer signed release and hands the restart, health
// check and rollback to the guard. See SPEC.md §2.
func (o *Ops) SelfUpdate(ctx context.Context, p rpc.SelfUpdateParams) (rpc.SelfUpdateResult, error) {
	if err := version.CanUpdate(); err != nil {
		return rpc.SelfUpdateResult{}, agentrpc.Invalid("self-update is not available in this build: %s", err.Error())
	}
	channel, ok := version.ParseChannel(p.Channel)
	if !ok {
		return rpc.SelfUpdateResult{}, agentrpc.Invalid("unknown release channel %q", p.Channel)
	}
	if !o.updateMu.TryLock() {
		return rpc.SelfUpdateResult{}, agentrpc.Conflict("an update is already running")
	}
	defer o.updateMu.Unlock()
	if g, err := o.Systemd.UnitProps(ctx, GuardUnit); err == nil && (g.ActiveState == "active" || g.ActiveState == "activating") {
		return rpc.SelfUpdateResult{}, agentrpc.Conflict("the previous update is still being applied")
	}

	tag, err := o.releaseTag(ctx, p.Version, channel)
	if err != nil {
		return rpc.SelfUpdateResult{}, err
	}
	base := version.ReleaseBase(tag)
	m, err := o.fetchManifest(ctx, base, tag, channel)
	if err != nil {
		return rpc.SelfUpdateResult{}, err
	}
	// The guard runs from the backup of the running agent.
	if fi, err := os.Lstat(filepath.Join(o.Paths.BinDir, "panel-agent")); err != nil || !fi.Mode().IsRegular() {
		return rpc.SelfUpdateResult{}, agentrpc.Conflict("no installed panel-agent to update from")
	}

	arts, err := o.stage(ctx, base, m)
	if err != nil {
		return rpc.SelfUpdateResult{}, err
	}
	plan, err := o.swap(arts, m.Version)
	if err != nil {
		return rpc.SelfUpdateResult{}, err
	}
	planPath := filepath.Join(o.Paths.WorkDir, planName)
	argv := []string{filepath.Join(o.Paths.BinDir, "panel-agent.bak"), "update-guard", planPath}
	err = plan.write(planPath)
	if err == nil {
		err = o.Systemd.StartTransient(ctx, GuardUnit, "CloudGround update guard: restart on "+m.Version+", roll back if unhealthy", argv)
	}
	if err != nil {
		if rerr := plan.restoreFiles(); rerr != nil {
			o.log().Error("self-update: the swap could not be undone", "err", rerr)
			return rpc.SelfUpdateResult{}, fmt.Errorf("start the update guard: %w; undoing the swap also failed: %v", err, rerr)
		}
		if errors.Is(err, host.ErrUnitExists) {
			return rpc.SelfUpdateResult{}, agentrpc.Conflict("the previous update is still being applied")
		}
		return rpc.SelfUpdateResult{}, fmt.Errorf("start the update guard (nothing was changed): %w", err)
	}
	o.log().Info("self-update handed to the guard", "version", m.Version, "previous", version.Version)
	return rpc.SelfUpdateResult{UpdatedTo: m.Version, Restarted: true}, nil
}

// releaseTag is the release to install: the one requested, or the newest
// the feed lists for channel.
func (o *Ops) releaseTag(ctx context.Context, requested, channel string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == "latest" {
		tag, err := version.Feed{Client: o.updateClient()}.Latest(ctx, channel)
		if err != nil {
			return "", fmt.Errorf("find the newest %s release: %w", channel, err)
		}
		return tag, nil
	}
	if !strings.HasPrefix(requested, "v") {
		requested = "v" + requested
	}
	if _, ok := version.ReleaseTag(requested); !ok {
		return "", agentrpc.Invalid("%q is not a release tag (vX.Y.Z or vX.Y.Z-beta.N)", requested)
	}
	return requested, nil
}

// fetchManifest downloads the signed manifest and checks it is the release
// asked for, suits the channel, and is newer than what runs and than
// anything installed here before.
func (o *Ops) fetchManifest(ctx context.Context, base, requested, channel string) (version.Manifest, error) {
	client := o.updateClient()
	body, err := fetchSmall(ctx, client, base+"/"+version.ManifestName, maxManifest)
	if err != nil {
		return version.Manifest{}, fmt.Errorf("download the release manifest: %w", err)
	}
	sig, err := fetchSmall(ctx, client, base+"/"+version.SignatureName, maxSignature)
	if err != nil {
		return version.Manifest{}, fmt.Errorf("download the release signature: %w", err)
	}
	m, err := version.VerifyManifest(body, sig)
	if err != nil {
		return version.Manifest{}, agentrpc.Invalid("%s", err.Error())
	}
	if err := version.CheckUpdate(m.Version, requested, version.Version); err != nil {
		return version.Manifest{}, agentrpc.Conflict("%s", err.Error())
	}
	if err := version.CheckChannel(channel, m.Version); err != nil {
		return version.Manifest{}, agentrpc.Conflict("%s", err.Error())
	}
	// A build that cannot order itself (a development build) is still
	// never taken below a release this server already ran.
	if high := HighestInstalled(o.Paths.StateDir()); high != "" && !version.Newer(m.Version, high) {
		return version.Manifest{}, agentrpc.Conflict("release %s is not newer than %s, already installed here: refusing to downgrade", m.Version, high)
	}
	for _, b := range Binaries {
		if _, ok := m.Sums[b]; !ok {
			return version.Manifest{}, agentrpc.Invalid("the signed manifest has no entry for %s", b)
		}
	}
	return m, nil
}

// stage downloads every artifact next to its live name and verifies it.
// Nothing live is touched; on any failure every staged file is removed.
func (o *Ops) stage(ctx context.Context, base string, m version.Manifest) ([]artifact, error) {
	var arts []artifact
	for _, b := range Binaries {
		live := filepath.Join(o.Paths.BinDir, b)
		arts = append(arts, artifact{name: b, live: live, staged: stagingName(live)})
	}
	for _, b := range OptionalBinaries {
		live := filepath.Join(o.Paths.BinDir, b)
		if fi, err := os.Lstat(live); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			// Still the link to panel-agent, which answers to the name: the
			// installer replaces it, a self-update leaves it (and its
			// rollback has nothing to put back).
			continue
		}
		if _, ok := m.Sums[b]; ok {
			arts = append(arts, artifact{name: b, live: live, staged: stagingName(live)})
		}
	}
	for _, u := range Units {
		if _, ok := m.Sums[u]; ok {
			live := filepath.Join(o.Paths.SystemdDir, u)
			arts = append(arts, artifact{name: u, live: live, staged: stagingName(live), unit: true})
		}
	}
	clean := func() {
		for _, a := range arts {
			os.Remove(a.staged)
		}
	}
	client := o.updateClient()
	for _, a := range arts {
		// A staged file left by an update that died is ours to replace.
		if err := os.Remove(a.staged); err != nil && !errors.Is(err, os.ErrNotExist) {
			clean()
			return nil, err
		}
		mode := os.FileMode(0o755)
		if a.unit {
			mode = 0o644
		}
		sum, err := fetchTo(ctx, client, base+"/"+a.name, a.staged, mode)
		if err != nil {
			clean()
			return nil, fmt.Errorf("download %s: %w", a.name, err)
		}
		if sum != m.Sums[a.name] {
			clean()
			return nil, agentrpc.Invalid("%s does not match the signed checksum: refusing to install", a.name)
		}
		if !a.unit {
			if err := nativeExecutable(a.staged); err != nil {
				clean()
				return nil, agentrpc.Wrap(agentrpc.CodeInvalid, a.name+" is not an executable for this server", err)
			}
		}
	}
	return arts, nil
}

// swap renames every staged artifact over its live name, keeping what it
// replaces: a hard link <binary>.bak for a binary (the running version), a
// copy in the work directory for a unit. An identical unit is left alone. On
// failure everything swapped so far is put back.
func (o *Ops) swap(arts []artifact, to string) (Plan, error) {
	plan := Plan{
		Version: to, Previous: version.Version,
		BinDir: o.Paths.BinDir, SystemdDir: o.Paths.SystemdDir,
		UnitBackup: filepath.Join(o.Paths.WorkDir, "update-backup"),
		APISocket:  o.Paths.APISocket, StateDir: o.Paths.StateDir(),
		StateDB: o.Paths.StateDB, Records: o.Paths.Records,
		SnapshotRoot: o.Paths.SnapshotRoot(), KeepSnapshots: DefaultKeepSnapshots,
	}
	fail := func(err error) (Plan, error) {
		for _, a := range arts {
			os.Remove(a.staged)
		}
		if rerr := plan.restoreFiles(); rerr != nil {
			o.log().Error("self-update: undoing a failed swap", "err", rerr)
		}
		return Plan{}, fmt.Errorf("install the release (undone): %w", err)
	}
	if err := rootfile.MkdirMode(plan.UnitBackup, 0o700); err != nil {
		return fail(err)
	}
	for _, a := range arts {
		if a.unit {
			cur, err := rootfile.ReadNoFollow(a.live)
			existed := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail(err)
			}
			next, err := os.ReadFile(a.staged)
			if err != nil {
				return fail(err)
			}
			if existed && bytes.Equal(cur, next) {
				os.Remove(a.staged)
				continue
			}
			if existed {
				if err := rootfile.Replace(filepath.Join(plan.UnitBackup, a.name), cur, 0o600); err != nil {
					return fail(err)
				}
			}
			if err := os.Rename(a.staged, a.live); err != nil {
				return fail(err)
			}
			if existed {
				plan.Units = append(plan.Units, a.name)
			} else {
				plan.FreshUnits = append(plan.FreshUnits, a.name)
			}
			if a.name == APISocketUnit {
				plan.SocketChanged = true
			}
			continue
		}
		bak := a.live + ".bak"
		if err := os.Remove(bak); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		fi, err := os.Lstat(a.live)
		existed := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if existed {
			if !fi.Mode().IsRegular() {
				return fail(fmt.Errorf("%s is not a regular file", a.live))
			}
			if err := os.Link(a.live, bak); err != nil {
				return fail(err)
			}
		}
		if err := os.Rename(a.staged, a.live); err != nil {
			if existed {
				os.Remove(bak)
			}
			return fail(err)
		}
		if existed {
			plan.Backed = append(plan.Backed, a.name)
		} else {
			plan.Fresh = append(plan.Fresh, a.name)
		}
	}
	return plan, nil
}
