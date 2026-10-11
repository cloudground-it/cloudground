package wp

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// Every wp-cli run of this package takes the run sandbox (phpfpm
// RunSandbox: the master's sandbox plus RestrictSUIDSGID, PrivateIPC and
// the @system-service filter), and the systemd-run argv carries each line
// as a -p property (R-10.5).
func TestWPCLIRunsAreHardened(t *testing.T) {
	root := siteTree(t)
	w := &fakeWP{}
	ops := newOps(t, w, &fakePHP{})
	s := wpSite(root)
	_, _ = ops.Run(context.Background(), s, "core", "version")
	_, _ = ops.Plugins(context.Background(), rpc.WPParams{Site: s})
	_, _ = ops.ObjectCache(context.Background(), rpc.WPParams{Site: s, Enable: true})
	w.mu.Lock()
	calls := append([]host.Invocation(nil), w.calls...)
	w.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no wp-cli run")
	}
	want := phpfpm.RunSandbox(s)
	for _, inv := range calls {
		if !slices.Equal(inv.Sandbox, want) {
			t.Errorf("%s: sandbox %q, want RunSandbox", strings.Join(inv.Args, " "), inv.Sandbox)
		}
		argv := host.ServiceArgs(host.WPService(inv, host.WPEnv))
		for _, line := range phpfpm.RunHardening {
			i := slices.Index(argv, line)
			if i < 1 || argv[i-1] != "-p" {
				t.Errorf("%s: argv lacks -p %s", strings.Join(inv.Args, " "), line)
			}
		}
	}
}
