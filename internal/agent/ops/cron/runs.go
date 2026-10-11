package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// A scheduled run ends in the runner, as root, out of the API's reach: the
// runner records how it ended in a file of its own per job
// (<RunDir>/<site user>-<job id>.json), and CronRuns reads them back for the
// API, which shows them as the job's last run. The panel used to know only
// of runs on demand, so a scheduled job's last run stayed empty.

// runFile is where job id of systemUser records its last scheduled run.
func runFile(dir, systemUser string, id int64) string {
	return filepath.Join(dir, systemUser+"-"+strconv.FormatInt(id, 10)+".json")
}

// recordRun writes how a scheduled run ended, replacing the job's last.
func recordRun(dir, systemUser string, id int64, run rpc.CronRun) error {
	if !site.ValidSystemUser(systemUser) || id <= 0 {
		return fmt.Errorf("invalid cron run %s/%d", systemUser, id)
	}
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if err := rootfile.MkdirMode(dir, 0o700); err != nil {
		return fmt.Errorf("the cron runs directory: %w", err)
	}
	return rootfile.Replace(runFile(dir, systemUser, id), b, 0o600)
}

// runOf is how a run with exit code code ended, as the API words it.
func runOf(at time.Time, code int) rpc.CronRun {
	status := "succeeded"
	if code != 0 {
		status = "failed"
	}
	return rpc.CronRun{At: at.UTC(), Status: status, ExitCode: code}
}

// Runs is CronRuns: the last scheduled run of each of the site's jobs that
// had one. A file that does not read as a run is skipped.
func (o *Ops) Runs(_ context.Context, p rpc.CronRunsParams) (rpc.CronRunsResult, error) {
	user := p.Site.SystemUser
	if !site.ValidSystemUser(user) {
		return rpc.CronRunsResult{}, agentrpc.Invalid("invalid system user %q", user)
	}
	out := rpc.CronRunsResult{Runs: map[string]rpc.CronRun{}}
	names, err := filepath.Glob(filepath.Join(o.runDir(), user+"-*.json"))
	if err != nil {
		return out, err
	}
	for _, name := range names {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(name), user+"-"), ".json")
		if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
			continue
		}
		b, err := rootfile.ReadNoFollow(name)
		if err != nil {
			continue
		}
		var run rpc.CronRun
		if json.Unmarshal(b, &run) != nil || run.At.IsZero() {
			continue
		}
		out.Runs[id] = run
	}
	return out, nil
}

// removeRuns deletes a site user's run records (its site is going).
func removeRuns(dir, systemUser string) error {
	if !site.ValidSystemUser(systemUser) {
		return agentrpc.Invalid("invalid system user %q", systemUser)
	}
	names, err := filepath.Glob(filepath.Join(dir, systemUser+"-*.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if err := os.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
