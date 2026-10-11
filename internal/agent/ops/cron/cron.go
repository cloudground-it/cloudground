// Package cron schedules a site's jobs and runs them, scheduled or on
// demand, each in a transient systemd service with the site's sandbox (the
// PHP master's, phpfpm.Sandbox) in the site's slice; and clears all of it
// when a site goes. SPEC.md in this directory is the contract.
package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Where the schedule lives.
const (
	// DefaultCrontabDir is cron's per-user spool on Debian and Ubuntu. Sites
	// had their crontab there before; Apply removes it.
	DefaultCrontabDir = "/var/spool/cron/crontabs"
	// DefaultCronDir holds one root-owned file per site (cron.d).
	DefaultCronDir = "/etc/cron.d"
	// DefaultSpecDir holds each site's jobs for the runner, root's only.
	DefaultSpecDir = "/etc/cloudground/cron"
	// DefaultBinDir is where panel-agent and the runner's link are.
	DefaultBinDir = "/usr/local/bin"
	// DefaultRunDir holds how each job's last scheduled run ended, one
	// file per job, written by the runner (root's only).
	DefaultRunDir = "/var/lib/cloudground-agent/cron-runs"
	// ClientName is the name panel-agent answers to as the scheduled
	// jobs' runner.
	ClientName  = "cg-site-cron"
	agentBinary = "panel-agent"
)

// RunLimit is the longest a job run on demand may take.
const RunLimit = 5 * time.Minute

// ScheduledLimit is the longest a scheduled run may take (RuntimeMaxSec):
// past it systemd stops the run with everything it started.
const ScheduledLimit = 6 * time.Hour

// outputLimit is how much of a run's output is returned: the tail.
const outputLimit = 64 << 10

// maxJobs bounds one site's schedule.
const maxJobs = 200

// path is the PATH of every job, scheduled or run now.
const path = "/usr/local/bin:/usr/bin:/bin"

// Ops holds what the handlers use.
type Ops struct {
	Exec    host.Exec
	Systemd host.Systemd
	// CrontabDir is cron's per-user spool ("" means DefaultCrontabDir).
	CrontabDir string
	// CronDir, SpecDir and BinDir: "" means the defaults.
	CronDir string
	SpecDir string
	BinDir  string
	// RunDir: "" means DefaultRunDir.
	RunDir string
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.ApplyCron, o.Apply)
	agentrpc.Handle(s, agentrpc.RunCron, o.Run)
	agentrpc.Handle(s, agentrpc.CronRuns, o.Runs)
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func (o *Ops) crontabDir() string { return or(o.CrontabDir, DefaultCrontabDir) }
func (o *Ops) cronDir() string    { return or(o.CronDir, DefaultCronDir) }
func (o *Ops) specDir() string    { return or(o.SpecDir, DefaultSpecDir) }
func (o *Ops) binDir() string     { return or(o.BinDir, DefaultBinDir) }
func (o *Ops) runDir() string     { return or(o.RunDir, DefaultRunDir) }

// cronFile is the site's file in cron.d: letters, digits and dashes only,
// as cron requires of the names it reads there.
func (o *Ops) cronFile(systemUser string) string {
	return filepath.Join(o.cronDir(), "cloudground-"+systemUser)
}

// SpecFile is a site's jobs for the runner.
func SpecFile(specDir, systemUser string) string {
	return filepath.Join(specDir, systemUser+".json")
}

// Spec is what the runner knows of a site's schedule: written by Apply,
// root's, read by the runner when cron starts it.
type Spec struct {
	User      string            `json:"user"`
	Domain    string            `json:"domain"`
	Root      string            `json:"root"`
	WordPress bool              `json:"wordpress"`
	Jobs      map[string]string `json:"jobs"` // id -> command, enabled jobs only
}

// Render is the site's cron.d file, its runner spec and the number of
// entries. Zero entries means the site has nothing scheduled.
func Render(s state.Site, jobs []cronjob.Job, binDir string) (string, Spec, int, error) {
	spec := Spec{User: s.SystemUser, Domain: s.Domain, Root: s.RootPath, Jobs: map[string]string{}}
	if len(jobs) > maxJobs {
		return "", spec, 0, agentrpc.Invalid("a site can have at most %d cron jobs", maxJobs)
	}
	sorted := append([]cronjob.Job(nil), jobs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	runner := filepath.Join(binDir, ClientName) + " " + s.SystemUser + " "
	var lines []string
	if s.Type == state.SiteWordPress || s.Type == state.SiteWooCommerce {
		spec.WordPress = true
		lines = append(lines, fmt.Sprintf("%d-59/5 * * * * root %swp", s.ID%5, runner))
	}
	for i, j := range sorted {
		if i > 0 && sorted[i-1].ID == j.ID {
			return "", spec, 0, agentrpc.Invalid("cron job %d is listed twice", j.ID)
		}
		// Every job is checked, enabled or not: the API stores what passed
		// here, and turning a job back on must not fail on its own text.
		if _, err := j.Line(); err != nil {
			return "", spec, 0, agentrpc.Invalid("cron job %d: %s", j.ID, err)
		}
		if j.Enabled {
			id := strconv.FormatInt(j.ID, 10)
			spec.Jobs[id] = j.Command
			lines = append(lines, strings.Join(strings.Fields(j.Schedule), " ")+" root "+runner+id)
		}
	}
	if len(lines) == 0 {
		return "", spec, 0, nil
	}
	var b strings.Builder
	b.WriteString("# CloudGround manages this file: change the site's cron jobs in the panel.\n")
	b.WriteString("MAILTO=\"\"\n")
	b.WriteString("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String(), spec, len(lines), nil
}

// Apply schedules the site's jobs from p.Jobs, or removes its schedule when
// nothing is left. The server holds the site's lock and has checked that
// the site still exists. A per-user crontab from before goes either way.
func (o *Ops) Apply(ctx context.Context, p rpc.CronParams) (rpc.CronResult, error) {
	content, spec, n, err := Render(p.Site, p.Jobs, o.binDir())
	if err != nil {
		return rpc.CronResult{}, err
	}
	if n == 0 {
		return rpc.CronResult{}, o.removeSchedule(p.Site.SystemUser)
	}
	if err := o.ensureRunnerLink(); err != nil {
		return rpc.CronResult{}, err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return rpc.CronResult{}, err
	}
	// The spec first: a new line of the cron.d file must find its job.
	if err := rootfile.MkdirMode(o.specDir(), 0o700); err != nil {
		return rpc.CronResult{}, fmt.Errorf("the cron spec directory: %w", err)
	}
	if err := rootfile.Replace(SpecFile(o.specDir(), p.Site.SystemUser), b, 0o600); err != nil {
		return rpc.CronResult{}, fmt.Errorf("the cron spec of %s: %w", p.Site.SystemUser, err)
	}
	if err := rootfile.Replace(o.cronFile(p.Site.SystemUser), []byte(content), 0o644); err != nil {
		return rpc.CronResult{}, fmt.Errorf("the cron file of %s: %w", p.Site.SystemUser, err)
	}
	if err := o.removeCrontab(p.Site.SystemUser); err != nil {
		return rpc.CronResult{}, err
	}
	return rpc.CronResult{Entries: n}, nil
}

// ensureRunnerLink makes <BinDir>/cg-site-cron a relative link to
// panel-agent.
func (o *Ops) ensureRunnerLink() error {
	link := filepath.Join(o.binDir(), ClientName)
	if t, err := os.Readlink(link); err == nil && t == agentBinary {
		return nil
	}
	if fi, err := os.Stat(filepath.Join(o.binDir(), agentBinary)); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the cron runner needs %s: %v", filepath.Join(o.binDir(), agentBinary), err)
	}
	tmp := link + ".cg-new"
	_ = os.Remove(tmp)
	if err := os.Symlink(agentBinary, tmp); err != nil {
		return fmt.Errorf("the cron runner link: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("the cron runner link: %w", err)
	}
	return nil
}

// UnitName is the transient unit a job runs in when run now.
func UnitName(systemUser string, id int64) string {
	return "cg-cron-" + systemUser + "-" + strconv.FormatInt(id, 10) + ".service"
}

// jobEnv is the environment of every job, scheduled or run now.
func jobEnv(root string) []string {
	return []string{"PATH=" + path, "HOME=" + root, "SHELL=/bin/sh", "TMPDIR=/tmp"}
}

// sandboxOf is the site's run sandbox: its PHP master's, hardened
// (phpfpm.RunSandbox).
func sandboxOf(domain, root string) []string {
	return phpfpm.RunSandbox(state.Site{Domain: domain, RootPath: root})
}

// resultRe finds systemd-run's result line. systemd 259 right-aligns the
// summary's labels ("          Finished with result: timeout"), so the
// line may start with blanks.
var resultRe = regexp.MustCompile(`(?m)^[ \t]*Finished with result: (\S+)`)

// Run runs one job now, as the site's user, in a transient service with the
// site's sandbox whose whole cgroup is killed when the job ends or
// overruns.
func (o *Ops) Run(ctx context.Context, p rpc.RunCronParams) (rpc.RunCronResult, error) {
	j := p.Job
	if j.ID <= 0 {
		return rpc.RunCronResult{}, agentrpc.Invalid("invalid cron job id %d", j.ID)
	}
	if err := cronjob.ValidateCommand(j.Command); err != nil {
		return rpc.RunCronResult{}, agentrpc.Invalid("cron job %d: %s", j.ID, err)
	}
	limit := RunLimit
	if p.MaxSeconds > 0 && time.Duration(p.MaxSeconds)*time.Second < limit {
		limit = time.Duration(p.MaxSeconds) * time.Second
	}
	user, root := p.Site.SystemUser, p.Site.RootPath
	unit := UnitName(user, j.ID)
	if st, err := o.Systemd.UnitProps(ctx, unit); err == nil && (st.ActiveState == "active" || st.ActiveState == "activating" || st.ActiveState == "deactivating") {
		return rpc.RunCronResult{}, agentrpc.Conflict("cron job %d is already running", j.ID)
	}
	svc := host.Service{
		Unit: unit, Description: fmt.Sprintf("CloudGround cron job %d of %s, run now", j.ID, p.Site.Domain),
		User: user, Slice: site.Slice(user), Dir: root, Env: jobEnv(root),
		Props: sandboxOf(p.Site.Domain, root), MaxRuntime: limit, Pipe: true,
		// Both streams, interleaved, on stdout; systemd-run's own lines
		// stay on stderr.
		Argv: []string{"/bin/sh", "-c", `exec 2>&1; /bin/sh -c "$1"`, "cg-cron", j.Command},
	}
	args := host.ServiceArgs(svc)
	// systemd-run's "Finished with result" line tells a timeout from an
	// exit status: drop --quiet.
	args = dropArg(args, "--quiet")
	// Only the tail is kept as the output arrives: a job may print without
	// end, and none of it may pile up in the agent.
	out := host.NewTailBuffer(outputLimit)
	res, err := o.Exec.Run(ctx, host.Cmd{Name: "systemd-run", Args: args, Timeout: limit + time.Minute, Stdout: out,
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}})
	r := rpc.RunCronResult{Output: string(out.Bytes()), ExitCode: -1}
	result := ""
	if m := resultRe.FindAllStringSubmatch(string(res.Stderr), -1); len(m) > 0 {
		result = m[len(m)-1][1]
	}
	var ee *host.ExitError
	switch {
	case err == nil:
		r.Status, r.ExitCode = "succeeded", 0
	case result == "timeout":
		r.Status = "timeout"
	case result == "exit-code" && errors.As(err, &ee):
		r.Status, r.ExitCode = "failed", ee.Code
	case errors.As(err, &ee) && result != "":
		r.Status = "failed"
	default:
		return rpc.RunCronResult{}, fmt.Errorf("run cron job %d of %s: %w", j.ID, p.Site.Domain, err)
	}
	return r, nil
}

func dropArg(args []string, drop string) []string {
	out := args[:0:0]
	for _, a := range args {
		if a != drop {
			out = append(out, a)
		}
	}
	return out
}

// RemoveSite stops the site user's job runs still going and removes its
// schedule. It is for site deletion, and works once the user is gone too.
func (o *Ops) RemoveSite(ctx context.Context, systemUser string) error {
	if !site.ValidSystemUser(systemUser) {
		return agentrpc.Invalid("invalid system user %q", systemUser)
	}
	var errs []error
	units, err := o.Systemd.ListUnits(ctx, "cg-cron-"+systemUser+"-*.service")
	if err != nil {
		errs = append(errs, err)
	}
	for _, u := range units {
		if err := o.Systemd.Stop(ctx, u.Name); err != nil && !errors.Is(err, host.ErrNoSuchUnit) {
			errs = append(errs, err)
		}
		_ = o.Systemd.ResetFailed(ctx, u.Name)
	}
	if err := o.removeSchedule(systemUser); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, removeRuns(o.runDir(), systemUser))
	return errors.Join(errs...)
}

// removeSchedule removes the site's cron.d file, its spec and a per-user
// crontab from before.
func (o *Ops) removeSchedule(systemUser string) error {
	if !site.ValidSystemUser(systemUser) {
		return agentrpc.Invalid("invalid system user %q", systemUser)
	}
	var errs []error
	for _, p := range []string{o.cronFile(systemUser), SpecFile(o.specDir(), systemUser)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	errs = append(errs, o.removeCrontab(systemUser))
	return errors.Join(errs...)
}

// removeCrontab unlinks the user's spool file: cron notices the spool
// directory change and drops the table.
func (o *Ops) removeCrontab(systemUser string) error {
	if !site.ValidSystemUser(systemUser) {
		return agentrpc.Invalid("invalid system user %q", systemUser)
	}
	if err := os.Remove(filepath.Join(o.crontabDir(), systemUser)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the crontab of %s: %w", systemUser, err)
	}
	return nil
}
