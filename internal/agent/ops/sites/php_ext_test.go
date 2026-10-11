package sites

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const fpmModules = `[PHP Modules]
apcu
Core
curl
mysqli
PDO
pdo_mysql
Zend OPcache

[Zend Modules]
Zend OPcache
`

// #29: a version's extensions are what its FPM binary loads; the common
// ones say whether they are among them (mysql is mysqli or pdo_mysql).
func TestPHPExtensionsListsWhatFPMLoads(t *testing.T) {
	e := newEnv(t)
	e.installPHP(t, "8.3")
	e.exec.Respond = func(c host.FakeCall) host.FakeReply { return host.FakeReply{Stdout: fpmModules} }
	got, err := e.PHPExtensions(context.Background(), rpc.PHPExtensionsParams{Version: "8.3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"apcu", "core", "curl", "mysqli", "pdo", "pdo_mysql", "zend opcache"}; !reflect.DeepEqual(got.Modules, want) {
		t.Fatalf("modules = %v, want %v", got.Modules, want)
	}
	installed := map[string]bool{}
	for _, x := range got.Extensions {
		installed[x.Name] = x.Installed
	}
	if !installed["apcu"] || !installed["mysql"] || !installed["curl"] || installed["redis"] || len(got.Extensions) != len(rpc.PHPExtensions) {
		t.Fatalf("extensions = %+v", got.Extensions)
	}
	calls := e.exec.Calls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0].Name, "/php-fpm8.3") || calls[0].Line() != calls[0].Name+" -m" {
		t.Fatalf("ran %+v", calls)
	}
	if _, err := e.PHPExtensions(context.Background(), rpc.PHPExtensionsParams{Version: "8.1"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a version not installed: %v", err)
	}
}

// Installing runs apt for php<v>-<ext> only, then restarts the masters on
// that version (read from their unit files) and no other.
func TestInstallPHPExtension(t *testing.T) {
	e := newEnv(t)
	e.installPHP(t, "8.3", "8.4")
	e.exec.Respond = func(c host.FakeCall) host.FakeReply {
		if c.Name == "apt-get" {
			return host.FakeReply{}
		}
		return host.FakeReply{Stdout: fpmModules + "redis\n"}
	}
	on83 := e.site(1, "a.example.com", state.SitePHP)
	on83.PHPVersion = "8.3"
	e.create(t, on83)
	e.create(t, e.site(2, "b.example.com", state.SitePHP))
	mark := len(e.systemd.Ops())
	got, err := e.InstallPHPExtension(context.Background(), rpc.PHPExtensionInstallParams{Version: "8.3", Extension: "redis"})
	if err != nil {
		t.Fatal(err)
	}
	var apt []string
	var env []string
	for _, c := range e.exec.Calls() {
		if c.Name == "apt-get" {
			apt = append(apt, c.Line())
			env = c.Env
		}
	}
	if want := []string{"apt-get -o DPkg::Lock::Timeout=120 install -y -qq --no-install-recommends php8.3-redis"}; !reflect.DeepEqual(apt, want) {
		t.Fatalf("apt = %v", apt)
	}
	if !strings.Contains(strings.Join(env, " "), "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("apt env = %v", env)
	}
	var restarts []string
	for _, op := range e.systemd.Ops()[mark:] {
		if strings.HasPrefix(op, "restart ") {
			restarts = append(restarts, op)
		}
	}
	if want := []string{"restart cloudground-php-cg-site-1.service"}; !reflect.DeepEqual(restarts, want) {
		t.Fatalf("restarts = %v, want %v", restarts, want)
	}
	redis := false
	for _, x := range got.Extensions {
		redis = redis || (x.Name == "redis" && x.Installed)
	}
	if !redis {
		t.Fatalf("after the install: %+v", got.Extensions)
	}
}

// The parameters refuse an extension off the list and a version that is
// not 8.x, before the agent runs anything.
func TestPHPExtensionParams(t *testing.T) {
	for _, p := range []rpc.PHPExtensionInstallParams{
		{Version: "8.3", Extension: "xdebug"},
		{Version: "8.3", Extension: "redis php8.3-evil"},
		{Version: "7.4", Extension: "redis"},
		{Version: "8.3;", Extension: "redis"},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	if err := (&rpc.PHPExtensionInstallParams{Version: "8.3", Extension: "intl"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
