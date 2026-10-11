package rpc

import (
	"fmt"

	"github.com/cloudground-it/cloudground/internal/blocklist"
)

// BlocklistFailure is the failed sign-ins the agent saw from one address
// (internal/blocklist SPEC §3): Source is "ssh" or "wordpress".
type BlocklistFailure struct {
	Address string `json:"address"`
	Source  string `json:"source"`
	Count   int    `json:"count"`
}

// BlocklistFailuresResult is every failure since the last read, which it
// takes away, and which of the agent's two readers are running.
type BlocklistFailuresResult struct {
	Failures []BlocklistFailure `json:"failures"`
	// SSHWatch: the journal of sshd is being read; NginxWatch: the vhosts'
	// failure log socket is open.
	SSHWatch   bool `json:"ssh_watch"`
	NginxWatch bool `json:"nginx_watch"`
}

// MaxBlocklistEntries bounds each list of BlocklistApplyParams.
const MaxBlocklistEntries = 100000

// BlocklistApplyParams is the whole block list as it is to be enforced
// now: the addresses and networks to drop and those never dropped.
type BlocklistApplyParams struct {
	Block []string `json:"block"`
	Allow []string `json:"allow"`
}

// Validate requires every entry to be an address or a CIDR, and every
// block to be no wider than /8 or /16 (internal/blocklist SPEC §1).
func (p *BlocklistApplyParams) Validate() error {
	if len(p.Block) > MaxBlocklistEntries || len(p.Allow) > MaxBlocklistEntries {
		return fmt.Errorf("at most %d entries in each list", MaxBlocklistEntries)
	}
	for _, s := range p.Block {
		pfx, err := blocklist.ParsePrefix(s)
		if err != nil {
			return err
		}
		if err := blocklist.CheckBlockWidth(pfx); err != nil {
			return err
		}
	}
	for _, s := range p.Allow {
		if _, err := blocklist.ParsePrefix(s); err != nil {
			return err
		}
	}
	return nil
}

// BlocklistApplyResult is what the netfilter table holds after the apply:
// how many prefixes each list became once those another covers were left
// out.
type BlocklistApplyResult struct {
	Blocked int `json:"blocked"`
	Allowed int `json:"allowed"`
}
