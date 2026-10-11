package agent

import (
	"fmt"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
)

// seedDatabases makes the fake server hold site_<from>..site_<to> and their
// accounts, which the agent's record of the sites' databases is rebuilt
// from on first use: site_<n> is site n's.
func seedDatabases(m *host.FakeMariaDB, from, to int) {
	var names []string
	for i := from; i <= to; i++ {
		names = append(names, fmt.Sprintf("site_%d", i))
	}
	m.Rows[db.ScanDatabasesQuery] = names
	m.Rows[db.ScanUsersQuery] = names
}
