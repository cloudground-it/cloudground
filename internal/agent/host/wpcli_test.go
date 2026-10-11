package host

import (
	"context"
	"slices"
	"strings"
	"testing"
)

var testSandbox = []string{"ProtectSystem=strict", "TemporaryFileSystem=/srv/sites:ro", "BindPaths=/srv/sites/x", "NoNewPrivileges=yes"}

func wpInv(args ...string) Invocation {
	return Invocation{User: "cg-site-3", Path: "/srv/sites/x/current", Args: args, Slice: "cg-site-3.slice", Sandbox: testSandbox}
}

// wp-cli runs the site's code, so it runs as the site's PHP does: a
// transient service in the site's slice with the site's sandbox, its
// output and exit status through systemd-run --pipe --wait (R-4.5 review).
func TestWPCLIRunsInTheSitesSandbox(t *testing.T) {
	t.Setenv("CLOUDGROUND_AGENT_TOKEN", "must-not-leak")
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: "6.8"} }}
	w := NewWPCLI(fx)
	out, err := w.Run(context.Background(), wpInv("core", "version"))
	if err != nil || string(out) != "6.8" {
		t.Fatalf("%q %v", out, err)
	}
	c := fx.Calls()[0]
	if c.Name != "systemd-run" {
		t.Fatalf("ran %q", c.Name)
	}
	for _, want := range []string{"--wait", "--pipe", "--uid=cg-site-3", "--gid=cg-site-3", "--slice=cg-site-3.slice",
		"--working-directory=/srv/sites/x/current", "RuntimeMaxSec=3000", "IgnoreSIGPIPE=no"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("argv lacks %q: %q", want, c.Line())
		}
	}
	for _, p := range testSandbox {
		i := slices.Index(c.Args, p)
		if i < 1 || c.Args[i-1] != "-p" {
			t.Errorf("sandbox line %q not passed with -p", p)
		}
	}
	for _, kv := range WPEnv {
		if !slices.Contains(c.Args, "--setenv="+kv) {
			t.Errorf("env %q not set", kv)
		}
	}
	if !strings.HasSuffix(c.Line(), " -- wp --path=/srv/sites/x/current core version") {
		t.Fatalf("command %q", c.Line())
	}
	if strings.Contains(strings.Join(c.Env, " ")+c.Line(), "must-not-leak") {
		t.Fatal("the agent's environment reached wp-cli")
	}
	if c.Stdin != "" {
		t.Fatal("stdin without a secret")
	}

	inv := wpInv("config", "create", "--prompt=dbpass")
	inv.Stdin = "s3cret"
	if _, err := w.Run(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	c = fx.Calls()[1]
	if c.Stdin != "s3cret\n" || strings.Contains(c.Line(), "s3cret") {
		t.Fatalf("secret handling: %+v", c)
	}
}

// A cancelled request stops the service: it is not systemd-run's child.
func TestWPCLIStopsTheServiceWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fx := &FakeExec{Respond: func(c FakeCall) FakeReply {
		if c.Name == "systemd-run" {
			cancel()
			return FakeReply{Code: 143}
		}
		return FakeReply{}
	}}
	if _, err := NewWPCLI(fx).Run(ctx, wpInv("core", "update")); err == nil {
		t.Fatal("no error")
	}
	calls := fx.Calls()
	if len(calls) != 2 || calls[1].Name != "systemctl" || calls[1].Args[0] != "stop" || !strings.HasPrefix(calls[1].Args[1], "cg-wp-cg-site-3-") {
		t.Fatalf("calls %v", calls)
	}
}

func TestWPCLIRejectsBadInvocations(t *testing.T) {
	fx := &FakeExec{}
	w := NewWPCLI(fx)
	bad := func(f func(*Invocation)) Invocation { inv := wpInv(); f(&inv); return inv }
	for _, inv := range []Invocation{
		bad(func(i *Invocation) { i.User = "root;x" }),
		bad(func(i *Invocation) { i.User = "-u" }),
		bad(func(i *Invocation) { i.Path = "relative" }),
		bad(func(i *Invocation) { i.Path = "/srv/a/../b" }),
		bad(func(i *Invocation) { i.Stdin = "two\nlines" }),
		bad(func(i *Invocation) { i.Slice = "" }),
		bad(func(i *Invocation) { i.Slice = "x;y" }),
		bad(func(i *Invocation) { i.Sandbox = nil }),
	} {
		if _, err := w.Run(context.Background(), inv); err == nil {
			t.Fatalf("%+v accepted", inv)
		}
	}
	if len(fx.Calls()) != 0 {
		t.Fatal("wp ran")
	}
}
