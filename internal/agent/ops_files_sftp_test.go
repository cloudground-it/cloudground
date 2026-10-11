package agent

import (
	"context"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/sftp"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// A new site's user starts in the SFTP deny group: no login, keys included,
// until the operator turns SFTP on.
func TestNewSiteHasSFTPOff(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := a.sitesHandlers().Create(context.Background(), rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	in, err := a.Accounts.(*host.FakeAccounts).IsMember(context.Background(), site.SystemUser, sftp.DenyGroup)
	if err != nil || !in {
		t.Fatalf("%s in %s = %v, %v; want true", site.SystemUser, sftp.DenyGroup, in, err)
	}
}
