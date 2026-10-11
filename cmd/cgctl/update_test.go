package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpanel "github.com/cloudground-it/cloudground/internal/agent/ops/panel"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// fakeAgent answers Ping with its version and, on SelfUpdate, writes the
// guard's verdict as the guard would.
type fakeAgent struct {
	version  string
	stateDir string
	verdict  agentpanel.Result
	updates  []rpc.SelfUpdateParams
}

func (a *fakeAgent) Call(_ context.Context, method string, params, out any) error {
	switch method {
	case "Ping":
		*(out.(*map[string]string)) = map[string]string{"version": a.version}
	case "SelfUpdate":
		p := params.(rpc.SelfUpdateParams)
		a.updates = append(a.updates, p)
		r := a.verdict
		r.Version, r.Previous = p.Version, a.version
		r.Started = time.Now().UTC().Format(time.RFC3339Nano)
		r.Finished = r.Started
		b, _ := json.Marshal(r)
		if err := os.WriteFile(filepath.Join(a.stateDir, agentpanel.ResultName), b, 0o600); err != nil {
			return err
		}
		*(out.(*rpc.SelfUpdateResult)) = rpc.SelfUpdateResult{UpdatedTo: p.Version, Restarted: true}
	default:
		return errors.New("unexpected " + method)
	}
	return nil
}

type fakeFeed struct{ latest map[string]string }

func (f fakeFeed) Latest(_ context.Context, ch string) (string, error) {
	if t, ok := f.latest[ch]; ok {
		return t, nil
	}
	return "", errors.New("no release")
}

func (f fakeFeed) FetchManifest(_ context.Context, tag string) (version.Manifest, error) {
	return version.Manifest{Version: tag}, nil
}

func newUpdater(t *testing.T, installed string) (*updater, *fakeAgent, *bytes.Buffer, *bytes.Buffer) {
	dir := t.TempDir()
	a := &fakeAgent{version: installed, stateDir: dir, verdict: agentpanel.Result{OK: true, Snapshot: dir + "/update-snapshots/x"}}
	var out, errb bytes.Buffer
	u := &updater{Agent: a, Feed: fakeFeed{latest: map[string]string{"stable": "v0.3.0", "beta": "v0.4.0-beta.1"}},
		StateDir: dir, StateDB: filepath.Join(dir, "state.db"), Now: time.Now, Sleep: func(time.Duration) {},
		Out: &out, Err: &errb, CanUpdate: func() error { return nil }}
	return u, a, &out, &errb
}

// v0.1.0 to the newest stable, several releases on: one SelfUpdate naming
// the release, then the guard's verdict.
func TestUpdateToTheNewest(t *testing.T) {
	u, a, out, errb := newUpdater(t, "v0.1.0")
	if code := u.run(context.Background(), updateFlags{timeout: time.Minute}); code != exitOK {
		t.Fatalf("exit %d: %s %s", code, out, errb)
	}
	if len(a.updates) != 1 || a.updates[0].Version != "v0.3.0" || a.updates[0].Channel != "stable" {
		t.Fatalf("updates %+v", a.updates)
	}
	if !strings.Contains(out.String(), "updated: v0.1.0 -> v0.3.0") || !strings.Contains(out.String(), "update-snapshots") {
		t.Fatalf("out: %s", out)
	}
}

// --check changes nothing; it says what would come.
func TestUpdateCheck(t *testing.T) {
	u, a, out, _ := newUpdater(t, "v0.1.0")
	if code := u.run(context.Background(), updateFlags{check: true, channel: "beta"}); code != exitOK || len(a.updates) != 0 {
		t.Fatalf("exit %d, updates %v", code, a.updates)
	}
	if !strings.Contains(out.String(), "update available: v0.1.0 -> v0.4.0-beta.1") {
		t.Fatalf("out: %s", out)
	}
	u, _, out, _ = newUpdater(t, "v0.3.0")
	if code := u.run(context.Background(), updateFlags{check: true}); code != exitOK || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("exit %d: %s", code, out)
	}
	// Asking for what runs is an answer, not a failure, and asks nothing.
	u, a, out, _ = newUpdater(t, "v0.3.0")
	if code := u.run(context.Background(), updateFlags{}); code != exitOK || len(a.updates) != 0 || !strings.Contains(out.String(), "up to date") {
		t.Fatalf("exit %d, updates %v: %s", code, a.updates, out)
	}
}

// An older release, or one below the newest ever installed here, is refused
// before the agent is asked; so is a beta on the stable channel.
func TestUpdateRefusesADowngrade(t *testing.T) {
	u, a, _, errb := newUpdater(t, "v0.3.0")
	if code := u.run(context.Background(), updateFlags{to: "v0.1.0"}); code != exitFailed || len(a.updates) != 0 {
		t.Fatalf("exit %d, updates %v", code, a.updates)
	}
	if !strings.Contains(errb.String(), "refusing to downgrade") {
		t.Fatalf("err: %s", errb)
	}
	u, a, _, errb = newUpdater(t, "dev")
	os.WriteFile(filepath.Join(u.StateDir, "highest-release"), []byte("v0.3.0\n"), 0o600)
	if code := u.run(context.Background(), updateFlags{to: "v0.2.0"}); code != exitFailed || len(a.updates) != 0 || !strings.Contains(errb.String(), "already installed here") {
		t.Fatalf("exit %d: %s", code, errb)
	}
	u, a, _, errb = newUpdater(t, "v0.1.0")
	if code := u.run(context.Background(), updateFlags{to: "v0.4.0-beta.1", channel: "stable"}); code != exitFailed || len(a.updates) != 0 || !strings.Contains(errb.String(), "beta") {
		t.Fatalf("exit %d: %s", code, errb)
	}
}

// A release the guard rolled back is a failure that says the previous one
// runs again, with its state.
func TestUpdateReportsARollback(t *testing.T) {
	u, a, _, errb := newUpdater(t, "v0.2.0")
	a.verdict = agentpanel.Result{RolledBack: true, RestoredHealthy: true, StateRestored: true, Snapshot: "/snap", Error: "not healthy within 1m0s: /healthz answered 503"}
	if code := u.run(context.Background(), updateFlags{timeout: time.Minute}); code != exitFailed {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"503", "rolled back: v0.2.0 runs again", "with its state from /snap"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("err lacks %q: %s", want, errb)
		}
	}
}

// A verdict older than this update is not this update's: the wait goes on
// until the timeout.
func TestUpdateIgnoresAnOldVerdict(t *testing.T) {
	u, _, _, _ := newUpdater(t, "v0.2.0")
	old := agentpanel.Result{OK: true, Version: "v0.2.0", Started: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), Finished: "x"}
	b, _ := json.Marshal(old)
	os.WriteFile(filepath.Join(u.StateDir, agentpanel.ResultName), b, 0o600)
	clock := time.Now()
	u.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	if _, err := u.waitResult(context.Background(), time.Now(), 5*time.Second); err == nil || !strings.Contains(err.Error(), "no verdict") {
		t.Fatalf("err = %v", err)
	}
}

// While a backup runs the update waits for it, and the agent is not asked.
func TestUpdateWaitsForABackup(t *testing.T) {
	u, a, _, errb := newUpdater(t, "v0.1.0")
	st, err := state.Open(u.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := st.CreateJob(state.Job{Kind: "backup.run", Status: state.JobQueued})
	if err == nil {
		_, err = st.StartJob(j.ID)
	}
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code := u.run(context.Background(), updateFlags{timeout: time.Minute}); code != exitFailed || len(a.updates) != 0 {
		t.Fatalf("exit %d, updates %v", code, a.updates)
	}
	if !strings.Contains(errb.String(), "backup.run") {
		t.Fatalf("err: %s", errb)
	}
	// The channel comes from the panel's settings when not given.
	st, _ = state.Open(u.StateDB)
	st.UpdatePanelSettings(func(p *state.PanelSettings) error { p.UpdateChannel = "beta"; return nil })
	st.Close()
	if ch := u.panelChannel(); ch != "beta" {
		t.Fatalf("channel %q", ch)
	}
}

func TestParseUpdateFlags(t *testing.T) {
	f, err := parseUpdateFlags([]string{"--to", "0.3.0", "--check"})
	if err != nil || f.to != "v0.3.0" || !f.check {
		t.Fatalf("%+v %v", f, err)
	}
	for _, bad := range [][]string{{"--to", "latest-and-greatest"}, {"--channel", "nightly"}, {"--frobnicate"}, {"--timeout", "-1s"}} {
		if _, err := parseUpdateFlags(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
