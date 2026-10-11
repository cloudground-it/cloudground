package rpc

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// SystemStatusResult is the machine's load and headroom, for the dashboard
// and the metric samples.
type SystemStatusResult struct {
	CPUCount       int     `json:"cpu_count"`
	Load1          float64 `json:"load1"`
	MemTotalMB     int64   `json:"mem_total_mb"`
	MemAvailableMB int64   `json:"mem_available_mb"`
	// The disk is the filesystem holding the sites.
	DiskTotalMB    int64  `json:"disk_total_mb"`
	DiskFreeMB     int64  `json:"disk_free_mb"`
	NginxRunning   bool   `json:"nginx_running"`
	MariaDBRunning bool   `json:"mariadb_running"`
	RedisRunning   bool   `json:"redis_running"`
	AgentVersion   string `json:"agent_version"`
	// UptimeSeconds is the machine's.
	UptimeSeconds   int64 `json:"uptime_seconds"`
	CPUHeadroomPct  int   `json:"cpu_headroom_pct"`
	MemHeadroomPct  int   `json:"mem_headroom_pct"`
	DiskHeadroomPct int   `json:"disk_headroom_pct"`
}

// ServiceParams names a managed service ("nginx", "mariadb", "redis", …;
// Capabilities lists them).
type ServiceParams struct {
	Name string `json:"name"`
}

// ServiceStatusResult is the services page: one row per listed service,
// then one for the sites' PHP masters.
type ServiceStatusResult struct {
	Services []ServiceInfo `json:"services"`
}

// ServiceInfo is one service as systemd sees it.
type ServiceInfo struct {
	Name    string `json:"name"`
	Unit    string `json:"unit"`
	Active  bool   `json:"active"`
	Enabled bool   `json:"enabled"`
	// State is "running", "stopped" (should run and does not), "failed",
	// or "disabled" (switched off on purpose).
	State string `json:"state"`
	// Detail is systemd's own ActiveState, or a summary for site-php.
	Detail string `json:"detail,omitempty"`
	// While running: the cgroup's memory and when the service started.
	MemoryBytes int64     `json:"memory_bytes,omitempty"`
	Since       time.Time `json:"since,omitempty"`
	// Version is what the program says of itself.
	Version string `json:"version,omitempty"`
}

// TailLogParams reads the last Lines lines of a log the agent knows by
// name: "nginx-access", "nginx-error", "php-error", "mariadb", "agent",
// "api", or "site:<domain>:access|error|php". Site, when given, must be the
// domain of a site source.
type TailLogParams struct {
	Source string `json:"source"`
	Site   string `json:"site,omitempty"`
	Lines  int    `json:"lines"`
}

// TailLogResult is the tail and where it was read ("journal:<unit>" for
// the journal).
type TailLogResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// FirewallStatusResult is ufw's state and its rules, as ufw prints them.
// While ufw is installed and off, Plan is what turning it on from the panel
// allows first (FirewallEnable), for the operator to read before.
type FirewallStatusResult struct {
	Enabled bool               `json:"enabled"`
	Rules   []string           `json:"rules"`
	Plan    []FirewallPlanRule `json:"plan,omitempty"`
}

// FirewallPlanRule is one rule FirewallEnable allows before `ufw enable`:
// the port, its protocol and what it carries (ssh, http, https, http3 or
// panel).
type FirewallPlanRule struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"`
	Service string `json:"service"`
}

// String is the rule as ufw writes it, "22/tcp".
func (r FirewallPlanRule) String() string { return fmt.Sprintf("%d/%s", r.Port, r.Proto) }

// FirewallEnableParams turns ufw on. Expect is the plan the operator was
// shown ("22/tcp", …): the agent refuses when its own plan has changed since
// (sshd moved to another port), so what is applied is what was read.
type FirewallEnableParams struct {
	Expect []string `json:"expect"`
}

// Validate requires the plan that was shown.
func (p *FirewallEnableParams) Validate() error {
	if len(p.Expect) == 0 {
		return errors.New("expect is required: the rules shown before enabling")
	}
	return nil
}

// FirewallRuleParams allows, denies or deletes a port, from anywhere or
// from one address or network. Proto is "tcp" or "udp"; anything else is
// taken as tcp.
type FirewallRuleParams struct {
	Action string `json:"action"`
	Port   int    `json:"port"`
	Proto  string `json:"proto"`
	From   string `json:"from,omitempty"`
}

// Validate requires a known action, a port and, when given, a source that
// is an IP address or a CIDR.
func (p *FirewallRuleParams) Validate() error {
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("invalid port %d", p.Port)
	}
	if p.From != "" && net.ParseIP(p.From) == nil {
		if _, _, err := net.ParseCIDR(p.From); err != nil {
			return fmt.Errorf("invalid source address %q", p.From)
		}
	}
	switch p.Action {
	case "allow", "deny", "delete":
		return nil
	}
	return fmt.Errorf("invalid action %q", p.Action)
}

// DriftParams maps each managed file to the SHA-256 the API recorded for
// it.
type DriftParams struct {
	Expected map[string]string `json:"expected"`
}

// DriftResult lists the managed files changed outside the panel.
type DriftResult struct {
	Drifted []DriftedFile `json:"drifted"`
	// NotManaged lists expected paths outside every place the panel
	// writes (records of an older layout): not hashed, and no longer
	// tracked.
	NotManaged []string `json:"not_managed,omitempty"`
}

// DriftedFile is a managed file whose content no longer has the recorded
// hash.
type DriftedFile struct {
	Path         string `json:"path"`
	ExpectedHash string `json:"expected_hash"`
	ActualHash   string `json:"actual_hash"`
}

// SelfUpdateParams installs the newest signed release. The release origin
// and the signing keys are built into the agent; nothing here can change
// them. Version, when set, is the release the operator was offered, and a
// signed manifest naming any other is refused. Channel is the panel's
// update_channel ("stable" when empty): it picks the newest release when no
// Version is given, and a beta is refused on the stable channel either way.
type SelfUpdateParams struct {
	Version string `json:"version"`
	Channel string `json:"channel,omitempty"`
}

// SelfUpdateResult is the version installed, and whether the services were
// restarted onto it.
type SelfUpdateResult struct {
	UpdatedTo string `json:"updated_to"`
	Restarted bool   `json:"restarted"`
}
