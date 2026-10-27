package agent

import (
	"context"
	"net/netip"

	"github.com/cloudground-it/cloudground/internal/agent/ops/blocklist"
)

// blocklistHandlers returns the agent's ops/blocklist handlers, built once:
// they keep the failures read since the panel last asked.
func (a *Agent) blocklistHandlers() *blocklist.Ops {
	a.blocklistOnce.Do(func() {
		a.blocklist = &blocklist.Ops{Exec: a.exec(), Socket: a.Paths.AuthLogSocket, Log: a.log(),
			// Resolved at call time: ops/sites is built on its own.
			Banned: func(ctx context.Context, block, allow []netip.Prefix) error {
				return a.sitesHandlers().SetBanned(ctx, block, allow)
			}}
	})
	return a.blocklist
}

// ServeBlocklist reads sshd's journal and the vhosts' failure log until ctx
// ends (ops/blocklist SPEC §1).
func (a *Agent) ServeBlocklist(ctx context.Context) {
	a.blocklistHandlers().Serve(ctx)
}
