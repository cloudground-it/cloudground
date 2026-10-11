//go:build hostintegration

// Apply, Run and RemoveSite against the real systemd (cron.d files, the
// sandboxed run now): a throwaway container booted with systemd as PID 1, as
// root.
//
//	go test -tags hostintegration -run IntegrationCronOps ./internal/agent/ops/cron/
package cron

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestIntegrationCronOps(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("systemd is not PID 1 here")
	}
	const user = "cg-site-90901"
	root := "/srv/sites/cgit-site.example.com"
	exec.Command("userdel", user).Run()
	os.RemoveAll(root)
	os.MkdirAll(root+"/logs", 0o755)
	if out, err := exec.Command("useradd", "--system", "--no-create-home", "--home-dir", root, "--shell", "/usr/sbin/nologin", user).CombinedOutput(); err != nil {
		t.Fatalf("useradd: %v %s", err, out)
	}
	exec.Command("chown", "-R", user+":", root).Run()
	t.Cleanup(func() { exec.Command("userdel", user).Run(); os.RemoveAll(root) })

	sd := host.NewSystemd()
	defer sd.Close()
	o := &Ops{Exec: host.OSExec{}, Systemd: sd, BinDir: t.TempDir(), CronDir: t.TempDir(), SpecDir: t.TempDir()}
	os.WriteFile(o.BinDir+"/"+agentBinary, []byte("#!/bin/sh\n"), 0o755)
	ctx := context.Background()
	site := state.Site{ID: 90901, Domain: "cgit-site.example.com", Type: state.SitePHP, SystemUser: user, RootPath: root}

	res, err := o.Apply(ctx, rpc.CronParams{Site: site, Jobs: []cronjob.Job{
		{ID: 1, Schedule: "*/5 * * * *", Command: "echo 50%", Enabled: true},
		{ID: 2, Schedule: "0 3 * * *", Command: "echo off"},
	}})
	if err != nil || res.Entries != 1 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	ct, _ := os.ReadFile(o.CronDir + "/cloudground-" + user)
	if !strings.Contains(string(ct), "cg-site-cron "+user+" 1\n") || strings.Contains(string(ct), " 2\n") {
		t.Fatalf("cron file:\n%s", ct)
	}

	run, err := o.Run(ctx, rpc.RunCronParams{Site: site, Job: cronjob.Job{ID: 1, Command: `id -un; pwd; echo "$HOME"; exit 7`}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.ExitCode != 7 || run.Output != user+"\n"+root+"\n"+root+"\n" {
		t.Fatalf("run %+v", run)
	}
	run, err = o.Run(ctx, rpc.RunCronParams{Site: site, Job: cronjob.Job{ID: 1, Command: "sleep 30"}, MaxSeconds: 1})
	if err != nil || run.Status != "timeout" {
		t.Fatalf("timeout run %+v %v", run, err)
	}

	if err := o.RemoveSite(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.CronDir + "/cloudground-" + user); err == nil {
		t.Fatal("cron file still there")
	}
}
