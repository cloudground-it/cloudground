package install

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const namespaceJournal = `cloudground-agent.service: Failed to set up mount namespacing: /var/lib/letsencrypt: No such file or directory
cloudground-agent.service: Failed at step NAMESPACE spawning /usr/local/bin/panel-agent: No such file or directory`

// A panel unit that never becomes active (a NAMESPACE failure restarts it
// for ever) fails the services step, which names the unit and shows the
// end of its journal, after waiting PanelWait: the install never reports
// success over a dead panel.
func TestServicesFailWhenAPanelUnitDoesNotStart(t *testing.T) {
	for _, unit := range panelUnits {
		t.Run(unit, func(t *testing.T) {
			m := newMachine(t, "26.04")
			o := m.options(false)
			o.SkipPackages = true
			m.sd.stuck[unit] = true
			m.journal[unit] = namespaceJournal
			slept := 0
			m.env.Sleep = func(time.Duration) { slept++ }
			m.listening[443] = true
			err := Apply(bg, m.env, Steps(m.env, o), nil)
			if err == nil {
				t.Fatal("the install succeeded with " + unit + " not running")
			}
			msg := err.Error()
			for _, want := range []string{"services:", unit + " is not active", "journalctl -u " + unit, "Failed at step NAMESPACE"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q:\n%s", want, msg)
				}
			}
			if slept < int(PanelWait/time.Second)-1 {
				t.Errorf("gave up after %d waits, want about %s", slept, PanelWait)
			}
		})
	}
}

// Both units active but /healthz failing (the API does not reach the
// agent) fails the step too, with both journals.
func TestServicesFailWhenTheHealthCheckFails(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.healthErr = errors.New("HTTP 503: the API does not reach the agent")
	m.journal["cloudground-agent"] = "agent: listening"
	m.journal["cloudground-api"] = "api: agent unreachable"
	m.listening[443] = true
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil {
		t.Fatal("the install succeeded with /healthz failing")
	}
	for _, want := range []string{"/healthz: HTTP 503", "journalctl -u cloudground-agent", "journalctl -u cloudground-api", "api: agent unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// A panel that comes up a few seconds late is waited for.
func TestServicesWaitForAPanelThatComesUp(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.sd.stuck["cloudground-agent"] = true
	slept := 0
	m.env.Sleep = func(time.Duration) {
		if slept++; slept == 5 {
			delete(m.sd.stuck, "cloudground-agent")
		}
	}
	m.listening[443] = true
	if err := Apply(bg, m.env, Steps(m.env, o), nil); err != nil {
		t.Fatal(err)
	}
}

// On an upgrade the same check runs after the restart: new binaries whose
// agent does not start fail the run. A re-run with nothing new finds the
// dead panel (its check is needed) and fails the same way; doctor reports it.
func TestUpgradeFailsWhenThePanelDoesNotComeBack(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	o.From = m.build("v1")
	m.mustApply(o)
	o.Upgrade = true

	o.From = m.build("v2")
	m.sd.stuck["cloudground-agent"] = true
	m.journal["cloudground-agent"] = namespaceJournal
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil || !strings.Contains(err.Error(), "cloudground-agent is not active") {
		t.Fatalf("upgrade with a dead agent: err = %v", err)
	}
	if !m.sd.did("restart cloudground-agent cloudground-api") {
		t.Errorf("the panel was not restarted: %v", m.sd.calls)
	}

	res := Plan(bg, Steps(m.env, o))
	for _, r := range res {
		if r.ID == "services" && (!r.Needed || !strings.Contains(r.Why, "cloudground-agent")) {
			t.Errorf("services check with a dead agent: needed=%v %q", r.Needed, r.Why)
		}
	}
	if err := Apply(bg, m.env, Steps(m.env, o), nil); err == nil || !strings.Contains(err.Error(), "Failed at step NAMESPACE") {
		t.Errorf("re-run with a dead agent: err = %v", err)
	}
}

// The journal of the API can carry the one-time setup link: it never
// reaches the error, which the terminal and the log show.
func TestPanelFailureLeavesOutTheSetupLink(t *testing.T) {
	m := newMachine(t, "24.04")
	o := m.options(false)
	o.SkipPackages = true
	m.sd.stuck["cloudground-api"] = true
	m.journal["cloudground-api"] = `{"msg":"setup","url":"https://<server-ip>:8443/setup/s3cr3t"}` + "\nboom"
	m.listening[443] = true
	err := Apply(bg, m.env, Steps(m.env, o), nil)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// The real probe: GET /healthz over the API's Unix socket, nil only on 200.
func TestPanelHealthOverTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var code atomic.Int32
	code.Store(http.StatusOK)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(code.Load()))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	if err := panelHealth(bg, sock); err != nil {
		t.Errorf("200: %v", err)
	}
	code.Store(http.StatusServiceUnavailable)
	if err := panelHealth(bg, sock); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	if err := panelHealth(bg, filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Error("no socket: no error")
	}
}

// A server whose agent fails at every start is still an upgrade (the token
// is there and the agent enabled): the re-run that repairs it is not
// refused for the ports its own nginx holds, and once the agent starts the
// run succeeds.
func TestRepairRunOverACrashLoopingAgent(t *testing.T) {
	m := newMachine(t, "26.04")
	o := m.options(false)
	o.SkipPackages = true
	m.mustApply(o)
	m.sd.stuck["cloudground-agent"] = true
	m.listening[80], m.listening[443] = true, true
	p, err := m.env.RunPreflight(bg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Upgrade || len(p.Problems) != 0 {
		t.Fatalf("crash-looping agent: upgrade=%v problems=%v", p.Upgrade, p.Problems)
	}
	delete(m.sd.stuck, "cloudground-agent")
	m.mustApply(o)
}
