package backup

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

const (
	forgetFiles = "abcd0000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	forgetDB    = "abcd1111bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	forgetOther = "abcd2222cccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// The site's snapshot list, as `snapshots --json --tag site:<domain>`
// answers it.
func (f *fixture) siteSnapshots(json string) { f.write("snap-site:docs.example.com.json", json) }

func (f *fixture) forget(id string) (rpc.ForgetSnapshotResult, error) {
	return f.o.ForgetSnapshot(ctx, rpc.ForgetSnapshotParams{Site: f.site, Repository: repoURL, Password: "pw", SnapshotID: id})
}

// A backup is deleted whole: the files snapshot named (by its short ID, as
// a record may hold it) and the database snapshot it pairs with, in one
// forget --prune, listed among the site's own snapshots only.
func TestForgetSnapshotForgetsTheBackupWithItsDatabase(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetFiles + `","tags":["site:docs.example.com","kind:full","db:` + forgetDB + `"]},` +
		`{"id":"` + forgetDB + `","tags":["site:docs.example.com","kind:full","part:database"]},` +
		`{"id":"` + forgetOther + `","tags":["site:docs.example.com","kind:full"]}]`)
	res, err := f.forget("abcd0000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Forgotten, " ") != forgetFiles+" "+forgetDB || res.Missing {
		t.Fatalf("forgotten = %+v", res)
	}
	calls := strings.Split(strings.TrimSpace(f.log()), "\n")
	if len(calls) != 2 || calls[0] != "snapshots --json --tag site:docs.example.com" ||
		calls[1] != "forget "+forgetFiles+" "+forgetDB {
		t.Fatalf("restic calls = %q", calls)
	}
}

// A deletion forgets without pruning; a files snapshot already gone takes
// the database half no files snapshot names any more with it; the prune
// is its own call.
func TestForgetMissingTakesTheOrphanDatabaseAndPruneIsSeparate(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetDB + `","tags":["site:docs.example.com","part:database"]},` +
		`{"id":"` + forgetOther + `","tags":["site:docs.example.com","db:abcd3333dddddddd"]},` +
		`{"id":"abcd3333dddddddd","tags":["site:docs.example.com","part:database"]}]`)
	res, err := f.forget(forgetFiles)
	if err != nil || !res.Missing || strings.Join(res.Forgotten, " ") != forgetDB {
		t.Fatalf("missing files snapshot = %+v %v, want its orphan database half forgotten", res, err)
	}
	if !strings.Contains(f.log(), "forget "+forgetDB+"\n") || strings.Contains(f.log(), "--prune") {
		t.Fatalf("restic calls = %q", f.log())
	}
	if _, err := f.o.PruneRepository(ctx, rpc.TestBackupParams{Repository: repoURL, Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(f.log()), "prune") {
		t.Fatalf("restic calls = %q", f.log())
	}
}

// A snapshot the site does not have is reported missing and nothing is
// forgotten; a backup's database half, an ambiguous short ID and an
// invalid ID are refused before restic forgets anything.
func TestForgetSnapshotRefusals(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetFiles + `","tags":["site:docs.example.com","db:` + forgetDB + `"]},` +
		`{"id":"` + forgetDB + `","tags":["site:docs.example.com","part:database"]}]`)
	if res, err := f.forget("ffffffff"); err != nil || !res.Missing || len(res.Forgotten) != 0 {
		t.Fatalf("missing snapshot = %+v %v", res, err)
	}
	if _, err := f.forget(forgetDB); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("the database half = %v, want invalid", err)
	}
	if _, err := f.forget("abcd"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("a short id = %v, want invalid", err)
	}
	if _, err := f.forget("--all-of-it"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("an option as id = %v, want invalid", err)
	}
	f.siteSnapshots(`[{"id":"abcd0000aaaa1111"},{"id":"abcd0000aaaa2222"}]`)
	if _, err := f.forget("abcd0000"); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("an ambiguous short id = %v, want invalid", err)
	}
	if strings.Contains(f.log(), "forget ") {
		t.Fatalf("a refused deletion forgot something: %s", f.log())
	}
	f.siteSnapshots("not json")
	if _, err := f.forget(forgetFiles); err == nil {
		t.Fatal("an unreadable snapshot list was accepted")
	}
	if _, err := f.o.ForgetSnapshot(ctx, rpc.ForgetSnapshotParams{Site: f.site, SnapshotID: forgetFiles}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Fatalf("no repository = %v, want invalid", err)
	}
}

// restic's typed failures keep their user-safe messages.
func TestForgetSnapshotTypedErrors(t *testing.T) {
	f := newFixture(t)
	f.siteSnapshots(`[{"id":"` + forgetFiles + `","tags":["site:docs.example.com"]}]`)
	f.write("exit-forget", "11") // restic: the repository is locked
	_, err := f.forget(forgetFiles)
	if agentrpc.CodeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("locked repository = %v (%s), want conflict", err, agentrpc.CodeOf(err))
	}
}
