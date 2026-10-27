package rpc

import (
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// DBServerParams is the connection to one external MariaDB or MySQL server
// (internal/api SPEC §6.5.1): what the agent connects with. AdminPassword is
// a secret: never logged.
type DBServerParams struct {
	// Kind is "mariadb" (also "", MySQL servers included) or "postgres".
	Kind string `json:"kind,omitempty"`
	// BackupDumps says whether a site's backup dumps the server's
	// databases (false: the provider backs them up). Always true for MariaDB.
	BackupDumps bool `json:"backup_dumps"`
	// ID is the panel's id for the server (0 for a server being tested
	// before it is registered). The box's own server is never one of these.
	ID            int64  `json:"id"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	TLS           bool   `json:"tls"`
	CACert        string `json:"ca_cert,omitempty"`
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`
}

// MaxCACert bounds a CA bundle.
const MaxCACert = 64 << 10

// CheckDBServerHost refuses a host that is not a host name or an address.
func CheckDBServerHost(h string) error {
	if h == "" || len(h) > 253 {
		return errors.New("the host is 1 to 253 characters")
	}
	if strings.ContainsFunc(h, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || strings.ContainsAny(h, "/@\\") {
		return errors.New("the host is a name or an address: no spaces, no / or @")
	}
	if strings.HasPrefix(h, "-") {
		return errors.New("the host cannot start with -")
	}
	return nil
}

// CheckDBServerCA refuses a CA bundle that is not PEM certificates.
func CheckDBServerCA(pem string) error {
	if pem == "" {
		return nil
	}
	if len(pem) > MaxCACert {
		return errors.New("the CA certificate is too large")
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(pem)) {
		return errors.New("the CA certificate is not PEM-encoded certificates")
	}
	return nil
}

// Kinds of external server.
const (
	KindMariaDB  = "mariadb"
	KindPostgres = "postgres"
)

// Validate holds the rules about the values themselves.
func (p *DBServerParams) Validate() error {
	switch p.Kind {
	case "", KindMariaDB:
	case KindPostgres:
		if !p.TLS {
			return errors.New("PostgreSQL needs TLS")
		}
	default:
		return fmt.Errorf("unknown kind of database server %q", p.Kind)
	}
	if err := CheckDBServerHost(p.Host); err != nil {
		return err
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("the port is 1 to 65535")
	}
	if p.AdminUser == "" || len(p.AdminUser) > 80 || strings.ContainsFunc(p.AdminUser, unicode.IsControl) {
		return errors.New("the administrator's user name is one line of 1 to 80 characters")
	}
	if p.AdminPassword == "" || strings.ContainsAny(p.AdminPassword, "\x00\r\n") {
		return errors.New("the administrator's password is required and has no line break")
	}
	return CheckDBServerCA(p.CACert)
}

// DBServerTestResult is a server that answered: its version.
type DBServerTestResult struct {
	Version string `json:"version"`
}

// SetDBServersParams replaces the agent's list of external servers (it keeps
// it in memory only).
type SetDBServersParams struct {
	Servers []DBServerParams `json:"servers"`
}

// MaxDBServers bounds the list.
const MaxDBServers = 50

// Validate checks every server, each with a positive, distinct id.
func (p *SetDBServersParams) Validate() error {
	if len(p.Servers) > MaxDBServers {
		return fmt.Errorf("at most %d database servers", MaxDBServers)
	}
	seen := map[int64]bool{}
	for i := range p.Servers {
		s := &p.Servers[i]
		if s.ID <= 0 || seen[s.ID] {
			return fmt.Errorf("invalid database server id %d", s.ID)
		}
		seen[s.ID] = true
		if err := s.Validate(); err != nil {
			return err
		}
	}
	return nil
}
