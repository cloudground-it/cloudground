package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/staging"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/version"
)

// fpmBinDir is a PHP binary directory holding the given entries: an
// executable file, a file without the execute bits, or a directory.
func fpmBinDir(t *testing.T, exec, plain, dirs []string) string {
	t.Helper()
	d := t.TempDir()
	for _, n := range exec {
		if err := os.WriteFile(filepath.Join(d, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range plain {
		if err := os.WriteFile(filepath.Join(d, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range dirs {
		if err := os.Mkdir(filepath.Join(d, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Capabilities reports what the fake machine has, through the RPC server,
// and touches nothing on it.
func TestCapabilities(t *testing.T) {
	a, h := testAgent(t)
	a.Paths.PHPBinDir = fpmBinDir(t,
		[]string{"php-fpm8.3", "php-fpm8.4", "php-fpm7.4", "php-fpm8.4.bak", "php8.5"},
		[]string{"php-fpm8.5"}, // not executable: cannot start a master
		[]string{"php-fpm8.2"}) // a directory
	h.nginx.Build = "nginx version: nginx/1.27.4\nconfigure arguments: --with-http_ssl_module --with-http_v3_module"
	h.exec.Respond = resticSays("restic 0.19.1 compiled with go1.24.4 on linux/amd64\n")
	c, err := rpcCall(a, agentrpc.Capabilities, agentrpc.None{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.PHPVersions, []string{"8.4", "8.3"}) {
		t.Errorf("php_versions = %v, want the executable FPM binaries, newest first", c.PHPVersions)
	}
	if !c.Features.HTTP3 {
		t.Error("an nginx built with http_v3_module serves HTTP/3")
	}
	if c.Paths.SitesRoot != a.Paths.SitesRoot || c.Paths.LogRoot != a.Paths.LogRoot {
		t.Errorf("paths = %+v", c.Paths)
	}
	if c.AgentVersion != version.Version || c.StartedAt.IsZero() {
		t.Errorf("agent %q started %v", c.AgentVersion, c.StartedAt)
	}
	var listed []string
	for _, s := range c.Services {
		if !s.Restartable {
			t.Errorf("%s: RestartService accepts every managed service", s.Name)
		}
		if s.Listed {
			listed = append(listed, s.Name)
		}
	}
	if !slices.Equal(listed, []string{"nginx", "mariadb", "redis"}) {
		t.Errorf("listed services = %v", listed)
	}
	if api, ok := c.Service("cloudground-api"); !ok || api.Listed || api.Unit != "cloudground-api" {
		t.Errorf("cloudground-api = %+v, %v", api, ok)
	}
	if c.ResticVersion != "restic 0.19.1 compiled with go1.24.4 on linux/amd64" || !c.ResticSupported {
		t.Errorf("restic %q supported %v", c.ResticVersion, c.ResticSupported)
	}
	// `restic version` is the one command run, and it changes nothing.
	for _, call := range h.exec.Calls() {
		if call.Name != "restic" || !slices.Equal(call.Args, []string{"version"}) {
			t.Errorf("reading the capabilities ran %s %v", call.Name, call.Args)
		}
	}
	if len(h.accounts.Ops()) != h.setup || len(h.systemd.Ops()) != 0 {
		t.Errorf("reading the capabilities changed the machine: %v %v", h.accounts.Ops(), h.systemd.Ops())
	}
}

// resticSays answers `restic version` with out, any other command as a
// missing binary.
func resticSays(out string) func(host.FakeCall) host.FakeReply {
	return func(c host.FakeCall) host.FakeReply {
		if c.Name == "restic" && slices.Equal(c.Args, []string{"version"}) {
			return host.FakeReply{Stdout: out}
		}
		return host.FakeReply{Err: errors.New("not found")}
	}
}

// A restic before 0.17 is reported and marked unsupported; one that cannot
// be run is an empty version, unsupported.
func TestCapabilitiesRestic(t *testing.T) {
	for _, c := range []struct {
		reply     func(host.FakeCall) host.FakeReply
		version   string
		supported bool
	}{
		{resticSays("restic 0.16.4 compiled with go1.22.2 on linux/amd64\n"), "restic 0.16.4 compiled with go1.22.2 on linux/amd64", false},
		{resticSays("restic 0.17.0\n"), "restic 0.17.0", true},
		{func(host.FakeCall) host.FakeReply { return host.FakeReply{Err: errors.New("restic: not found")} }, "", false},
		{resticSays("something else\n"), "", false},
	} {
		a, h := testAgent(t)
		h.exec.Respond = c.reply
		got, err := a.capabilities(t.Context(), agentrpc.None{})
		if err != nil {
			t.Fatal(err)
		}
		if got.ResticVersion != c.version || got.ResticSupported != c.supported {
			t.Errorf("want %q %v, got %q %v", c.version, c.supported, got.ResticVersion, got.ResticSupported)
		}
	}
}

// No FPM binary at all is an empty list (never null), and an nginx without
// the module has no HTTP/3.
func TestCapabilitiesBareMachine(t *testing.T) {
	a, h := testAgent(t)
	a.Paths.PHPBinDir = fpmBinDir(t, nil, nil, nil)
	h.nginx.Build = "nginx version: nginx/1.24.0"
	c, err := rpcCall(a, agentrpc.Capabilities, agentrpc.None{})
	if err != nil {
		t.Fatal(err)
	}
	if c.PHPVersions == nil || len(c.PHPVersions) != 0 || c.Features.HTTP3 {
		t.Fatalf("php %#v http3 %v", c.PHPVersions, c.Features.HTTP3)
	}
	raw := mustJSON(t, c)
	if !strings.Contains(raw, `"php_versions":[]`) {
		t.Fatalf("php_versions is not an empty array: %s", raw)
	}
}

// The push policy the API is told is the one the agent applies: for every
// table, the first rule that covers it gives staging.Protected's answer.
func TestCapabilitiesProtectedTablesArePushPolicy(t *testing.T) {
	a, _ := testAgent(t)
	c, err := a.capabilities(t.Context(), agentrpc.None{})
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{"wp_users", "WP_USERMETA", "wp_posts", "wp_postmeta", "wp_comments", "wp_options",
		"wp_wc_orders", "wp_woocommerce_sessions", "wp_actionscheduler_actions", "wp_terms", "shop_wc_reserved_stock"}
	for _, store := range []bool{false, true} {
		for _, tb := range tables {
			want := staging.Protected(tb, store)
			got := ""
			for _, r := range c.ProtectedTables {
				if (!r.StoresOnly || store) && r.Protects(tb) {
					got = r.Reason
					break
				}
			}
			if got != want {
				t.Errorf("%s (store %v): rules say %q, the push policy %q", tb, store, got, want)
			}
		}
	}
	if staging.Protected("wp_posts", false) != "" || staging.Protected("wp_users", false) != "user accounts" ||
		staging.Protected("wp_wc_order_stats", true) != "live store data (orders, customers, payments)" {
		t.Fatal("the push policy itself changed")
	}
}
