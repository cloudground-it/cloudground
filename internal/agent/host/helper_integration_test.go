//go:build hostintegration

// HelperExec against a real systemd: a throwaway container booted with
// systemd as PID 1, run as root, with a panel-agent built for it
// (CG_TEST_AGENT_BINARY), and the directories the agent unit's sandbox
// names (HelperProps' ReadWritePaths) created.
//
//	go build -o /usr/local/bin/panel-agent ./cmd/panel-agent
//	CG_TEST_AGENT_BINARY=/usr/local/bin/panel-agent go test -tags hostintegration -run IntegrationHelper ./internal/agent/host/
package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func helperForTest(t *testing.T) HelperExec {
	requireSystemd(t)
	bin := os.Getenv("CG_TEST_AGENT_BINARY")
	if bin == "" {
		t.Skip("CG_TEST_AGENT_BINARY is not set")
	}
	HelperNames["sh"] = true
	t.Cleanup(func() { delete(HelperNames, "sh") })
	return HelperExec{Exec: OSExec{}, AgentBinary: bin}
}

// The tool runs in its own unit of cg-helper.slice, with the environment
// and the stdin it was given, and its exit status comes back under its name.
func TestIntegrationHelperRunsInItsUnit(t *testing.T) {
	h := helperForTest(t)
	res, err := h.Run(context.Background(), Cmd{Name: "sh",
		Args: []string{"-c", `cat /proc/self/cgroup; echo "x=$X"; echo "in=$(cat)"; echo oops >&2; exit 3`},
		Env:  []string{"PATH=/usr/bin:/bin", "X=from-the-agent"}, Stdin: strings.NewReader("data"), Timeout: time.Minute})
	out := string(res.Stdout)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Name != "sh" || ee.Code != 3 || !strings.Contains(ee.Stderr, "oops") {
		t.Fatalf("err = %v; stdout %q", err, out)
	}
	if !strings.Contains(out, "/cg.slice/cg-helper.slice/cg-helper-sh-") {
		t.Errorf("not in a helper unit: %q", out)
	}
	if !strings.Contains(out, "x=from-the-agent\n") || !strings.Contains(out, "in=data\n") {
		t.Errorf("environment or stdin lost: %q", out)
	}
}

// A cancelled helper with Interrupt gets SIGINT, and has ended (its unit
// gone) when the call returns with the context's error.
func TestIntegrationHelperCancelInterrupts(t *testing.T) {
	h := helperForTest(t)
	mark := "/run/cg-helper-int-" + time.Now().Format("150405.000")
	defer os.Remove(mark)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(2*time.Second, cancel)
	_, err := h.Run(ctx, Cmd{Name: "sh", Interrupt: 5 * time.Second,
		Args: []string{"-c", `trap 'touch ` + mark + `; exit 130' INT; while :; do sleep 0.1; done`}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the tool was not interrupted: %v", err)
	}
	if n := procsOf("cg-helper-int"); n != 0 {
		t.Errorf("%d processes left", n)
	}
}

// A helper that passes its memory limit is killed in its own unit; the
// caller gets a failure, not a hang, and lives on.
func TestIntegrationHelperOOMStaysInItsUnit(t *testing.T) {
	h := helperForTest(t)
	defer func(p []string) { HelperProps = p }(HelperProps)
	HelperProps = append(HelperProps[:len(HelperProps):len(HelperProps)], "MemoryMax=32M", "MemorySwapMax=0")
	start := time.Now()
	_, err := h.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", `x=a; while :; do x="$x$x"; done`}, Timeout: 2 * time.Minute})
	var ee *ExitError
	if !errors.As(err, &ee) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	t.Logf("killed after %v: %v", time.Since(start), err)
}
