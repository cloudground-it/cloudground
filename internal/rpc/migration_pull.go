package rpc

import (
	"errors"
	"net"
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// PullParams names an old host to pull a site from (#82; ops/migration
// SPEC §8). Password or PrivateKey: exactly one; they are single-use and
// never stored.
type PullParams struct {
	Site       state.Site `json:"site"`
	Host       string     `json:"host"`
	Port       int        `json:"port,omitempty"`
	User       string     `json:"user"`
	Password   string     `json:"password,omitempty"`
	PrivateKey string     `json:"private_key,omitempty"`
	// HostKey pins the host's key: its SHA-256 fingerprint, "SHA256:…".
	HostKey string `json:"host_key,omitempty"`
	// Path is the old site's folder on the host.
	Path string `json:"path"`
	// DB is "" (none), "dump" (mysqldump over SSH) or "file" (SQLPath).
	DB        string `json:"db,omitempty"`
	SQLPath   string `json:"sql_path,omitempty"`
	ReleaseID string `json:"release_id,omitempty"`
}

// Sites lists the site pulled into.
func (p *PullParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

var pullUserRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,64}$`)
var pullHostRe = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)

// Validate checks the request before any connection (internal/api SPEC
// §10.1).
func (p *PullParams) Validate() error {
	host := strings.Trim(p.Host, "[]")
	switch {
	case host == "" || strings.HasPrefix(host, "-") || (net.ParseIP(host) == nil && !pullHostRe.MatchString(host)):
		return errors.New("host: give a host name or an address")
	case p.Port < 0 || p.Port > 65535:
		return errors.New("port must be between 1 and 65535")
	case !pullUserRe.MatchString(p.User) || strings.HasPrefix(p.User, "-"):
		return errors.New("user: give the account's name")
	case (p.Password == "") == (p.PrivateKey == ""):
		return errors.New("give a password or a private key")
	case p.HostKey != "" && !strings.HasPrefix(p.HostKey, "SHA256:"):
		return errors.New("host_key: give the SHA256: fingerprint")
	case strings.TrimSpace(p.Path) == "" || strings.ContainsAny(p.Path, "\x00\n\r"):
		return errors.New("path: give the old site's folder")
	}
	switch p.DB {
	case "", "dump":
	case "file":
		if !strings.HasSuffix(strings.ToLower(p.SQLPath), ".sql") || strings.ContainsAny(p.SQLPath, "\x00\n\r") {
			return errors.New("sql_path: give the path of a .sql file on the old host")
		}
	default:
		return errors.New("db must be empty, dump or file")
	}
	if p.ReleaseID != "" && !site.ValidReleaseID(p.ReleaseID) {
		return errors.New("invalid release id")
	}
	return nil
}

// PullEstimateResult is a dry run's measure of the old site.
type PullEstimateResult struct {
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	WordPress bool   `json:"wordpress"`
	Shell     bool   `json:"shell"`
	HostKey   string `json:"host_key"`
	FreeBytes int64  `json:"free_bytes"`
	Fits      bool   `json:"fits"`
}

// PullResult is what a pull wrote, relative to the site's root.
type PullResult struct {
	Archive string `json:"archive"`
	SQL     string `json:"sql,omitempty"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	HostKey string `json:"host_key"`
}

// PullCleanupParams removes what a pull wrote for a release.
type PullCleanupParams struct {
	Site      state.Site `json:"site"`
	ReleaseID string     `json:"release_id"`
}

// Sites lists the site cleaned.
func (p *PullCleanupParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a release id.
func (p *PullCleanupParams) Validate() error {
	if !site.ValidReleaseID(p.ReleaseID) {
		return errors.New("invalid release id")
	}
	return nil
}
