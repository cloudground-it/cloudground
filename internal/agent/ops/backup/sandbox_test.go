package backup

import (
	"context"
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

// sandboxSpy records the sandboxed calls and runs them with the stand-in.
type sandboxSpy struct {
	*host.ResticCLI
	got []host.TreeSandbox
}

func (s *sandboxSpy) RunSandboxed(ctx context.Context, repo host.Repo, sb host.TreeSandbox, args ...string) ([]byte, error) {
	s.got = append(s.got, sb)
	return s.ResticCLI.RunSandboxed(ctx, repo, sb, args...)
}

// The files backup, the one restic call that walks the tenant's tree, runs
// sandboxed on the site's root, with the cache and the local repository
// writable (SPEC §4); the database dump does not walk a tree and does not.
func TestFilesBackupRunsSandboxed(t *testing.T) {
	f := newFixture(t)
	spy := &sandboxSpy{ResticCLI: f.o.Restic.(*host.ResticCLI)}
	f.o.Restic = spy
	f.o.CacheDir = t.TempDir()
	if _, err := f.backup("full"); err != nil {
		t.Fatal(err)
	}
	if len(spy.got) != 1 {
		t.Fatalf("sandboxed calls: %+v", spy.got)
	}
	sb := spy.got[0]
	if sb.Tree != f.site.RootPath || !slices.Equal(sb.Writable, []string{f.o.CacheDir, repoURL}) {
		t.Errorf("sandbox: %+v", sb)
	}
}
