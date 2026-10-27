package rpc

import (
	"strings"
	"time"
)

// Capabilities is the machine as the agent finds it (SPEC.md §4): the API
// asks for these facts instead of keeping its own copy of them.
type Capabilities struct {
	AgentVersion string `json:"agent_version"`
	// StartedAt is when the agent process started.
	StartedAt time.Time `json:"started_at"`
	// PHPVersions are the versions a site can run, newest first.
	PHPVersions []string `json:"php_versions"`
	// Services are the managed services, in the services page's order.
	Services []ServiceCapability `json:"services"`
	// ProtectedTables is the staging push policy.
	ProtectedTables []TableRule `json:"protected_tables"`
	Paths           HostPaths   `json:"paths"`
	Features        Features    `json:"features"`
	// ResticVersion is restic's own `restic version` line; empty when the
	// agent could not run it (restic missing or broken).
	ResticVersion string `json:"restic_version"`
	// ResticSupported: restic is 0.17 or later, the first with typed exit
	// codes (a locked repository, a wrong password); an older one backs up,
	// but its failures all read the same.
	ResticSupported bool `json:"restic_supported"`
	// Runtimes are the Node.js and Python versions installed (#28).
	Runtimes RuntimesResult `json:"runtimes"`
}

// HasPHP reports whether version is installed. An empty list (the agent
// found no FPM binary at all, a layout it does not know) answers yes, as
// the agent then refuses no version either.
func (c Capabilities) HasPHP(version string) bool {
	if len(c.PHPVersions) == 0 {
		return true
	}
	for _, v := range c.PHPVersions {
		if v == version {
			return true
		}
	}
	return false
}

// Service returns the named managed service.
func (c Capabilities) Service(name string) (ServiceCapability, bool) {
	for _, s := range c.Services {
		if s.Name == name {
			return s, true
		}
	}
	return ServiceCapability{}, false
}

// ServiceCapability is one managed service: Listed is a row of the
// services page, Restartable a name RestartService accepts.
type ServiceCapability struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Listed      bool   `json:"listed"`
	Restartable bool   `json:"restartable"`
}

// TableRule protects the tables whose lower-cased name ends with (Match
// "suffix") or contains ("contains") Pattern. StoresOnly rules apply only
// when production is a store.
type TableRule struct {
	Match      string `json:"match"`
	Pattern    string `json:"pattern"`
	Reason     string `json:"reason"`
	StoresOnly bool   `json:"stores_only,omitempty"`
}

// Protects reports whether the rule covers table.
func (r TableRule) Protects(table string) bool {
	t := strings.ToLower(table)
	switch r.Match {
	case "suffix":
		return strings.HasSuffix(t, r.Pattern)
	case "contains":
		return strings.Contains(t, r.Pattern)
	}
	return false
}

// HostPaths are the places of the agent's layout the API names.
type HostPaths struct {
	// SitesRoot holds every site's tree, SitesRoot/<domain>.
	SitesRoot string `json:"sites_root"`
	// LogRoot holds every site's logs, LogRoot/<domain>/.
	LogRoot string `json:"log_root"`
	// MySQLSocket is the local MariaDB server's Unix socket, which a
	// site's own programs may connect on (shown in a new site's summary).
	MySQLSocket string `json:"mysql_socket,omitempty"`
}

// Features are optional abilities of the installed programs.
type Features struct {
	// HTTP3: nginx was built with QUIC, so vhosts listen on 443/udp.
	HTTP3 bool `json:"http3"`
}
