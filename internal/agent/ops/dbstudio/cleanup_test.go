package dbstudio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #78 (SPEC §8.4): a dry run counts and changes nothing, the number of
// revisions kept is checked and reaches the statement as a number, a static
// site is refused, and a real run writes the site's cleanup log.
func TestCleanup(t *testing.T) {
	fx := newFixture(t)
	fx.wpFn = func(args []string) ([]byte, error) { return []byte("wp_\n"), nil }
	fx.f.selectRows = [][]*string{{sp("11")}, {sp("12")}, {sp("13")}}

	for _, keep := range []int{0, -1, 101} {
		if _, err := fx.o.Cleanup(ctx, rpc.DBStudioCleanupParams{Site: fx.site, KeepRevisions: keep, DryRun: true}); code(err) != "invalid" {
			t.Errorf("keep %d: %v", keep, err)
		}
	}
	static := fx.site
	static.Type = state.SiteStatic
	if _, err := fx.o.Cleanup(ctx, rpc.DBStudioCleanupParams{Site: static, KeepRevisions: 5, DryRun: true}); code(err) != "invalid" {
		t.Errorf("a static site: %v", err)
	}

	res, err := fx.o.Cleanup(ctx, rpc.DBStudioCleanupParams{Site: fx.site, KeepRevisions: 7, DryRun: true})
	if err != nil || res.Executed || res.ExcessRevisions != 3 || res.ExpiredTransients != 3 || res.DeletedRevisions != 0 || res.DeletedTransients != 0 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if sent := fx.sent(); !strings.Contains(sent, "PARTITION BY post_parent ORDER BY post_date DESC, ID DESC") || !strings.Contains(sent, "WHERE r.rn > 7") {
		t.Fatalf("no statement ranks the revisions of each post and keeps 7: %s", sent)
	}
	for _, args := range fx.wp {
		if args[0] != "db" {
			t.Fatalf("a dry run ran wp %v", args)
		}
	}

	// A real run deletes through wp-cli, in batches, with IDs the server gave,
	// and logs its result.
	fx.o.LogRoot = filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(filepath.Join(fx.o.LogRoot, fx.site.Domain), 0o750); err != nil {
		t.Fatal(err)
	}
	fx.wp = nil
	fx.wpFn = func(args []string) ([]byte, error) {
		if args[0] == "post" {
			// The revisions are gone: the next query finds none.
			fx.f.mu.Lock()
			fx.f.selectRows = nil
			fx.f.mu.Unlock()
		}
		return []byte("wp_\n"), nil
	}
	res, err = fx.o.Cleanup(ctx, rpc.DBStudioCleanupParams{Site: fx.site, KeepRevisions: 7})
	if err != nil || !res.Executed || res.DeletedRevisions != 3 {
		t.Fatalf("run: %+v %v", res, err)
	}
	var deleted bool
	for _, args := range fx.wp {
		if args[0] == "post" && args[1] == "delete" {
			deleted = true
			if got := strings.Join(args, " "); got != "post delete 11 12 13 --force" {
				t.Errorf("wp %s", got)
			}
		}
	}
	if !deleted {
		t.Fatalf("no revision was deleted: %v", fx.wp)
	}
	log, err := os.ReadFile(filepath.Join(fx.o.LogRoot, fx.site.Domain, "cleanup.log"))
	if err != nil || !strings.Contains(string(log), "cleanup:") || !strings.Contains(string(log), "beyond 7 per post") {
		t.Fatalf("log %q %v", log, err)
	}
}
