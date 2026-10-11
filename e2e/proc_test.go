//go:build e2e

package e2e

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var netDialer = net.Dialer{Timeout: 10 * time.Second}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// resticOn returns the suite's own look into a repository: restic (bin, the
// real one) on repo with password pw. --no-cache: restic otherwise needs a
// cache directory, from $XDG_CACHE_HOME or $HOME, and refuses to run
// without one ("unable to locate cache directory"); a suite run as a
// systemd unit has neither, and every check that read the repository failed
// (the agent's restic has a cache of its own). The step is recorded without
// the password.
func (h *harness) resticOn(bin, repo, pw string) func(args ...string) result {
	return func(args ...string) result {
		h.step("restic " + strings.Join(args, " "))
		return runEnv([]string{"RESTIC_PASSWORD=" + pw}, bin, append([]string{"--no-cache", "-r", repo}, args...)...)
	}
}

// runEnv is run with extra environment variables (RESTIC_PASSWORD=…);
// callers record the step themselves so the secret is not traced.
func runEnv(extra []string, name string, args ...string) result {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{Out: out.String(), Err: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	default:
		r.Code, r.Err = -1, err.Error()
	}
	return r
}

// bg is a background command (`cmd &` in the bash suite).
type bg struct {
	cmd  *exec.Cmd
	once sync.Once
}

func execBackground(name string, args ...string) *bg {
	cmd := exec.Command(name, args...)
	b := &bg{cmd: cmd}
	if err := cmd.Start(); err != nil {
		b.cmd = nil
	}
	return b
}

// stop is `kill $PID; wait $PID`.
func (b *bg) stop() {
	b.once.Do(func() {
		if b.cmd == nil || b.cmd.Process == nil {
			return
		}
		_ = b.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = b.cmd.Process.Kill()
			<-done
		}
	})
}
