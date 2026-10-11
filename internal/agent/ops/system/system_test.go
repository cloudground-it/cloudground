package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
)

// fixture is an Ops over fakes, with every path in a temp directory.
type fixture struct {
	*Ops
	sd   *host.FakeSystemd
	exec *host.FakeExec
	dir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sd := host.NewFakeSystemd()
	ex := &host.FakeExec{}
	o := &Ops{
		Systemd:  sd,
		Exec:     ex,
		Firewall: host.NewFirewall(ex),
		Paths: Paths{
			SitesRoot:     mk("srv"),
			LogRoot:       mk("logs"),
			NginxLogDir:   mk("nginx-logs"),
			MariaDBLogDir: mk("mysql-logs"),
			SSHDConfig:    filepath.Join(mk("ssh"), "sshd_config"),
			NginxSites:    mk("sites-enabled"),
			NginxConfDir:  mk("conf.d"),
			MariaDBConf:   mk("mariadb.conf.d"),
			PHPMasterDir:  mk("php-masters"),
			NginxAuthDir:  mk("auth"),
			SystemdDir:    mk("systemd"),
		},
	}
	return &fixture{Ops: o, sd: sd, exec: ex, dir: dir}
}

// respond answers calls by program name.
func (f *fixture) respond(replies map[string]host.FakeReply) {
	f.exec.Respond = func(c host.FakeCall) host.FakeReply { return replies[c.Name] }
}

func codeOf(err error) agentrpc.Code {
	if err == nil {
		return ""
	}
	return agentrpc.CodeOf(err)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every method of this package is registered with the RPC server.
func TestRegisterServesEveryMethod(t *testing.T) {
	f := newFixture(t)
	s := agentrpc.NewServer(agentrpc.Config{SitesRoot: f.Paths.SitesRoot})
	f.Register(s)
	got := map[string]bool{}
	for _, m := range s.Missing() {
		got[m] = true
	}
	for _, m := range []string{"SystemStatus", "ServiceStatus", "RestartService", "TailLog", "FirewallStatus", "FirewallRule", "CheckDrift"} {
		if got[m] {
			t.Errorf("%s is not registered", m)
		}
	}
	if _, e := s.Invoke(context.Background(), "ServiceStatus", []byte(`{}`)); e != nil {
		t.Fatalf("ServiceStatus through the server: %v", e)
	}
}
