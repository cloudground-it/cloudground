package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A scheduled run is recorded by the runner and read back by CronRuns: the
// panel only knew of runs on demand, so a scheduled job's last run stayed
// empty however often it ran.
func TestScheduledRunsAreRecorded(t *testing.T) {
	o, _, _ := newOps(t)
	s := wpSite()
	ctx := context.Background()
	if res, err := o.Runs(ctx, rpc.CronRunsParams{Site: s}); err != nil || len(res.Runs) != 0 {
		t.Fatalf("before any run: %+v %v", res, err)
	}

	// The runner's own path: the command's status is the run's, and the
	// exit code comes back.
	if code := runAndRecord("/bin/sh", []string{"systemd-run", "-c", "exit 0"}, nil, o.RunDir, s.SystemUser, 7); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := runAndRecord("/bin/sh", []string{"systemd-run", "-c", "exit 3"}, nil, o.RunDir, s.SystemUser, 8); code != 3 {
		t.Fatalf("exit %d", code)
	}
	// Another site's run, and a file that is not a run, are not this site's.
	if err := recordRun(o.RunDir, "cg-site-120", 7, runOf(time.Now(), 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.RunDir, "cg-site-12-9.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := o.Runs(ctx, rpc.CronRunsParams{Site: s})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 2 || res.Runs["7"].Status != "succeeded" || res.Runs["8"].Status != "failed" || res.Runs["8"].ExitCode != 3 ||
		time.Since(res.Runs["7"].At) > time.Minute {
		t.Fatalf("runs %+v", res.Runs)
	}
	if fi, err := os.Stat(runFile(o.RunDir, s.SystemUser, 7)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("run file: %v %v", fi, err)
	}

	// The site's deletion takes its records, not another site's.
	if err := o.RemoveSite(ctx, s.SystemUser); err != nil {
		t.Fatal(err)
	}
	if res, _ := o.Runs(ctx, rpc.CronRunsParams{Site: s}); len(res.Runs) != 0 {
		t.Fatalf("runs left after the site went: %+v", res.Runs)
	}
	if _, err := os.Stat(runFile(o.RunDir, "cg-site-120", 7)); err != nil {
		t.Fatalf("another site's run went too: %v", err)
	}
	if _, err := o.Runs(ctx, rpc.CronRunsParams{Site: state.Site{SystemUser: "../etc"}}); err == nil {
		t.Fatal("a bad user name reached the files")
	}
}
