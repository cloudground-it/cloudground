package staging

import (
	"context"
	"slices"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The copy's database and user reach wp-cli's argv only in their one form,
// site_<staging id> (R-10.5): an option-like or another site's name is
// refused, nothing of it run.
func TestCreateRefusesAnUnexpectedDatabaseUser(t *testing.T) {
	for name, user := range map[string]string{"an option": "--require=/tmp/x.php", "another site's": "site_3", "not a name": "Site_4"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.production(t, false)
			stg := f.stg
			stg.Config.Database.User = user
			_, err := f.Create(context.Background(), rpc.StagingParams{Production: f.prod, Staging: stg, DBPassword: "pw"})
			if agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
				t.Fatalf("err = %v", err)
			}
			if f.got() != "" || len(f.wp.lines()) != 0 {
				t.Fatalf("something ran: %s %v", f.got(), f.wp.lines())
			}
		})
	}
}

// The staging copy's wp-cli steps (configuration, salts, the search-replace)
// run in the run sandbox (phpfpm.RunSandbox, R-10.5).
func TestStagingWPCLIRunsAreHardened(t *testing.T) {
	f := newFixture(t)
	f.production(t, true)
	f.maria.Dumps["site_3"] = "-- production\n"
	if _, err := f.Create(context.Background(), rpc.StagingParams{Production: f.prod, Staging: f.stg, DBPassword: "db-secret"}); err != nil {
		t.Fatal(err)
	}
	f.wp.mu.Lock()
	defer f.wp.mu.Unlock()
	if len(f.wp.calls) == 0 {
		t.Fatal("no wp-cli run")
	}
	for _, inv := range f.wp.calls {
		if !slices.Equal(inv.Sandbox, phpfpm.RunSandbox(f.stg)) {
			t.Errorf("%v: sandbox %q", inv.Args, inv.Sandbox)
		}
	}
}
