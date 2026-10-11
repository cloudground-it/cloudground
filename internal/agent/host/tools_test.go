package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func envKey(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- restic --

func TestResticSecretsTravelInTheEnvironment(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "inherited-must-go")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "inherited-must-go")
	t.Setenv("RESTIC_CACHE_DIR", "/var/cache/cloudground/restic")
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: "[]"} }}
	r := NewRestic(fx)
	repo := Repo{URL: "s3:https://key:secret@example/bucket", Password: "pw", Env: map[string]string{
		"AWS_ACCESS_KEY_ID": "id", "AWS_SECRET_ACCESS_KEY": "sk", "LD_PRELOAD": "/evil.so", "B2_ACCOUNT_KEY": "a\nb",
	}}
	out, err := r.Run(context.Background(), repo, "snapshots", "--json")
	if err != nil || string(out) != "[]" {
		t.Fatalf("%q %v", out, err)
	}
	c := fx.Calls()[0]
	if c.Line() != "restic snapshots --json" {
		t.Fatalf("argv %q", c.Line())
	}
	// Stopped with SIGINT first, so it removes its repository lock.
	if c.Interrupt != ResticGrace || ResticGrace <= 0 {
		t.Fatalf("restic runs with Interrupt %v, want %v", c.Interrupt, ResticGrace)
	}
	for _, a := range c.Args {
		if strings.Contains(a, "secret") || a == "pw" {
			t.Fatalf("secret in argv: %v", c.Args)
		}
	}
	for _, want := range []string{"RESTIC_REPOSITORY=" + repo.URL, "RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=id",
		"AWS_SECRET_ACCESS_KEY=sk", "RESTIC_CACHE_DIR=/var/cache/cloudground/restic"} {
		if !envHas(c.Env, want) {
			t.Fatalf("env lacks %s", want)
		}
	}
	if envHas(c.Env, "RESTIC_PASSWORD=inherited-must-go") || envHas(c.Env, "AWS_SECRET_ACCESS_KEY=inherited-must-go") ||
		envKey(c.Env, "LD_PRELOAD=") || envKey(c.Env, "B2_ACCOUNT_KEY=") {
		t.Fatalf("env leaks: %v", c.Env)
	}
}

func TestResticVersion(t *testing.T) {
	for out, want := range map[string][3]int{
		"restic 0.19.1 compiled with go1.24.4 on linux/amd64\n": {0, 19, 1},
		"restic 0.16.4 compiled with go1.22.2 on linux/amd64":   {0, 16, 0},
		"restic 1.0.0 compiled with go1.30 on linux/amd64":      {1, 0, 1},
	} {
		fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: out} }}
		v, err := ResticVersion(context.Background(), fx)
		if err != nil || v.Major != want[0] || v.Minor != want[1] || v.TypedExitCodes() != (want[2] == 1) {
			t.Errorf("%q: %+v %v", out, v, err)
		}
		if fx.Calls()[0].Line() != "restic version" {
			t.Errorf("ran %q", fx.Calls()[0].Line())
		}
	}
	for _, bad := range []string{"", "rustic 0.19", "restic x.y"} {
		if _, err := ParseResticVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestResticExitCodes(t *testing.T) {
	for code, want := range map[int]error{3: ErrIncomplete, 10: ErrRepoNotFound, 11: ErrRepoLocked, 12: ErrWrongPassword, 130: ErrInterrupted} {
		fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Code: code, Stderr: "whatever"} }}
		_, err := NewRestic(fx).Run(context.Background(), Repo{URL: "/r", Password: "p"}, "cat", "config")
		var ee *ExitError
		if !errors.Is(err, want) || !errors.As(err, &ee) || ee.Code != code || ExitCode(err) != code {
			t.Fatalf("exit %d: err = %v", code, err)
		}
	}
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Code: 1} }}
	_, err := NewRestic(fx).Run(context.Background(), Repo{URL: "/r", Password: "p"}, "check")
	if ExitCode(err) != 1 || errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := NewRestic(fx).Run(context.Background(), Repo{URL: "/r"}, "check"); err == nil {
		t.Fatal("ran without a password")
	}
}

func TestResticDecoders(t *testing.T) {
	backup := `{"message_type":"status","percent_done":0.5}
{"message_type":"summary","files_new":3,"data_added":1024,"total_files_processed":3,"total_bytes_processed":2048,"snapshot_id":"abcdef0123456789"}
`
	s, err := DecodeBackupSummary([]byte(backup))
	if err != nil || s.SnapshotID != "abcdef0123456789" || s.TotalBytesProcessed != 2048 || s.FilesNew != 3 {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := DecodeBackupSummary([]byte(`{"message_type":"status"}`)); err == nil {
		t.Fatal("no summary decoded as one")
	}
	snaps, err := DecodeSnapshots([]byte(`[{"id":"aa","short_id":"a","time":"2026-10-08T19:00:00Z","tags":["site:x","part:database"]}]` + "\n"))
	if err != nil || len(snaps) != 1 || snaps[0].Tags[1] != "part:database" || snaps[0].Time.IsZero() {
		t.Fatalf("%+v %v", snaps, err)
	}
	plan := "repository 1234 opened\nApplying Policy: keep within 30d\n" +
		`[{"tags":["site:x"],"keep":[{"id":"k1","tags":["site:x"]}],"remove":[{"id":"r1"},{"id":"r2"}]}]` + "\n"
	groups, err := DecodeForgetPlan([]byte(plan))
	if err != nil || len(groups) != 1 || groups[0].Keep[0].ID != "k1" || len(groups[0].Remove) != 2 {
		t.Fatalf("%+v %v", groups, err)
	}
	if _, err := DecodeForgetPlan([]byte("nothing here\n")); err == nil {
		t.Fatal("no plan decoded as one")
	}
}

// ----------------------------------------------------------------- wpcli --

// -------------------------------------------------------------- firewall --

func TestUFWArgsAndValidation(t *testing.T) {
	fx := &FakeExec{}
	u := NewFirewall(fx)
	ctx := context.Background()
	if err := u.Allow(ctx, FirewallRule{Port: 8443, Proto: "tcp"}); err != nil {
		t.Fatal(err)
	}
	if err := u.Deny(ctx, FirewallRule{Port: 25, Proto: "tcp", From: "203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	if err := u.Delete(ctx, FirewallRule{Port: 8443, Proto: "udp"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ufw allow 8443/tcp",
		"ufw deny from 203.0.113.0/24 to any port 25 proto tcp",
		"ufw delete deny 8443/udp",
		"ufw delete allow 8443/udp",
	}
	calls := fx.Calls()
	for i, w := range want {
		if calls[i].Line() != w {
			t.Fatalf("call %d %q, want %q", i, calls[i].Line(), w)
		}
		if !envHas(calls[i].Env, "LC_ALL=C") {
			t.Fatal("ufw not run in the C locale")
		}
	}
	for _, bad := range []FirewallRule{{Port: 0, Proto: "tcp"}, {Port: 70000, Proto: "tcp"}, {Port: 80, Proto: "icmp"}, {Port: 80, Proto: "tcp", From: "any; rm"}} {
		if err := u.Allow(ctx, bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if len(fx.Calls()) != 4 {
		t.Fatal("ufw ran for an invalid rule")
	}
}

func TestUFWDeleteDeny(t *testing.T) {
	reply := "Rule deleted\n"
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: reply} }}
	u := NewFirewall(fx)
	r := FirewallRule{Port: 8443, Proto: "tcp", From: "203.0.113.4"}
	if ok, err := u.DeleteDeny(context.Background(), r); err != nil || !ok {
		t.Fatalf("deleted: %v %v", ok, err)
	}
	reply = "Could not delete non-existent rule\n"
	if ok, err := u.DeleteDeny(context.Background(), r); err != nil || ok {
		t.Fatalf("non-existent read as deleted: %v %v", ok, err)
	}
	// An inactive ufw (measured on 24.04) only rewrites its stored rules
	// and says so; the deny is gone all the same.
	reply = "WARN: initcaps\n\nRules updated\n"
	if ok, err := u.DeleteDeny(context.Background(), r); err != nil || !ok {
		t.Fatalf("inactive ufw's deletion read as none: %v %v", ok, err)
	}
	// A rule of both families found in one only.
	reply = "Could not delete non-existent rule\nRules updated (v6)\n"
	if ok, err := u.DeleteDeny(context.Background(), r); err != nil || !ok {
		t.Fatalf("v6-only deletion read as none: %v %v", ok, err)
	}
	reply = "Could not delete non-existent rule\nCould not delete non-existent rule (v6)\n"
	if ok, err := u.DeleteDeny(context.Background(), r); err != nil || ok {
		t.Fatalf("non-existent in both families read as deleted: %v %v", ok, err)
	}
	calls := fx.Calls()
	if len(calls) != 5 || calls[0].Line() != "ufw delete deny from 203.0.113.4 to any port 8443 proto tcp" {
		t.Fatalf("calls %v", calls)
	}
}

func TestUFWStatus(t *testing.T) {
	out := "Status: active\n\nTo                         Action      From\n--                         ------      ----\n22/tcp                     ALLOW       Anywhere\n443                        ALLOW       Anywhere\n"
	u := NewFirewall(&FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: out} }})
	st, err := u.Status(context.Background())
	if err != nil || !st.Active || len(st.Rules) != 2 || !strings.HasPrefix(st.Rules[0], "22/tcp") {
		t.Fatalf("%+v %v", st, err)
	}
	u = NewFirewall(&FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: "Status: inactive\n"} }})
	if st, _ := u.Status(context.Background()); st.Active {
		t.Fatal("inactive read as active")
	}
	// Inactive, ufw keeps its rules: `ufw status` prints none of them, so
	// they come from `ufw show added` (the page showed "no rules").
	stored := "Added user rules (see 'ufw status' for running firewall):\nufw allow 22/tcp\nufw allow 8443/tcp\nufw deny from 203.0.113.4 to any port 8443 proto tcp\n"
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		if strings.Join(c.Args, " ") == "show added" {
			return FakeReply{Stdout: stored}
		}
		return FakeReply{Stdout: "Status: inactive\n"}
	}}
	st, err = NewFirewall(fx).Status(context.Background())
	want := []string{"allow 22/tcp", "allow 8443/tcp", "deny from 203.0.113.4 to any port 8443 proto tcp"}
	if err != nil || st.Active || !reflect.DeepEqual(st.Rules, want) {
		t.Fatalf("inactive with stored rules: %+v %v", st, err)
	}
	fx = &FakeExec{Respond: func(c FakeCall) FakeReply {
		if strings.Join(c.Args, " ") == "show added" {
			return FakeReply{Stdout: "Added user rules (see 'ufw status' for running firewall):\n(None)\n"}
		}
		return FakeReply{Stdout: "Status: inactive\n"}
	}}
	if st, err := NewFirewall(fx).Status(context.Background()); err != nil || len(st.Rules) != 0 {
		t.Fatalf("inactive without rules: %+v %v", st, err)
	}
	u = NewFirewall(&FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Err: &exec.Error{Name: "ufw", Err: exec.ErrNotFound}} }})
	if _, err := u.Status(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// ------------------------------------------------------------------ acme --

func TestCertbotArgs(t *testing.T) {
	args, err := CertOnlyArgs(CertRequest{Name: "example.com", Domains: []string{"example.com", "www.example.com"},
		Email: "ops@example.com", Webroot: "/var/www/acme", Staging: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "certonly --webroot --webroot-path /var/www/acme --non-interactive --agree-tos --email ops@example.com --cert-name example.com -d example.com -d www.example.com --staging" {
		t.Fatalf("argv %q", got)
	}
	args, err = CertOnlyArgs(CertRequest{Name: "example.com", Domains: []string{"example.com", "*.example.com"},
		Email: "ops@example.com", CloudflareCredentials: "/etc/letsencrypt/cf.ini"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "certonly --dns-cloudflare --dns-cloudflare-credentials /etc/letsencrypt/cf.ini --dns-cloudflare-propagation-seconds 30 --non-interactive --agree-tos --email ops@example.com --cert-name example.com -d example.com -d *.example.com" {
		t.Fatalf("argv %q", got)
	}
	for _, bad := range []CertRequest{
		{Name: "example.com", Domains: []string{"example.com"}, Email: "-x@y.z", Webroot: "/w"},
		{Name: "example.com", Domains: []string{"example.com"}, Email: "Name <a@b.c>", Webroot: "/w"},
		{Name: "--help", Domains: []string{"example.com"}, Email: "a@b.c", Webroot: "/w"},
		{Name: "example.com", Domains: []string{"-d"}, Email: "a@b.c", Webroot: "/w"},
		{Name: "example.com", Domains: []string{"example.com"}, Email: "a@b.c"},
		{Name: "example.com", Domains: []string{"example.com"}, Email: "a@b.c", Webroot: "/w", CloudflareCredentials: "/c"},
	} {
		if _, err := CertOnlyArgs(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// Deleting a lineage that does not exist is decided from certbot's state,
// not from its output, and certbot is not run.
func TestCertbotDelete(t *testing.T) {
	dir := t.TempDir()
	fx := &FakeExec{}
	c := &Certbot{Exec: fx, ConfigDir: dir}
	if err := c.Delete(context.Background(), "example.com"); !errors.Is(err, ErrNoSuchCert) {
		t.Fatalf("err = %v", err)
	}
	if len(fx.Calls()) != 0 {
		t.Fatal("certbot ran for a missing lineage")
	}
	os.MkdirAll(filepath.Join(dir, "renewal"), 0o755)
	os.WriteFile(filepath.Join(dir, "renewal", "example.com.conf"), nil, 0o644)
	if err := c.Delete(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if fx.Calls()[0].Line() != "certbot delete --cert-name example.com --non-interactive" {
		t.Fatal(fx.Calls())
	}
	if err := c.Delete(context.Background(), "../x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

// The panel's lineage has no dot; it must still be a valid name, and a path
// or an option still must not.
func TestCertbotLineageNames(t *testing.T) {
	for _, ok := range []string{"cloudground-panel", "example.com", "a.b.example.com"} {
		if _, err := CertOnlyArgs(CertRequest{Name: ok, Domains: []string{"example.com"}, Email: "a@b.c", Webroot: "/w"}); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "a..b", "../x", "a/b", "Example.com", "x-", "a.b."} {
		if _, err := CertOnlyArgs(CertRequest{Name: bad, Domains: []string{"example.com"}, Email: "a@b.c", Webroot: "/w"}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}
