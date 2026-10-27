package host

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// RemoteServer is an external MariaDB or MySQL server the agent
// administers over the network (host SPEC, mariadb: a remote server).
type RemoteServer struct {
	ID            int64
	Host          string
	Port          int
	TLS           bool
	CACert        string // PEM; empty with TLS means the system's authorities
	AdminUser     string
	AdminPassword string // a secret: never in an error, a log or an argument
	// Kind is "mariadb" or "postgres"; BackupDumps whether a site's backup
	// dumps the server's databases (the provider backs it up otherwise).
	Kind        string
	BackupDumps bool
}

// Addr is host:port.
func (r RemoteServer) Addr() string { return net.JoinHostPort(r.Host, strconv.Itoa(r.Port)) }

// TLSConfig is the client configuration of a TLS server: the certificate is
// verified for Host against CACert, or the system's authorities.
func (r RemoteServer) TLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{ServerName: r.Host, MinVersion: tls.VersionTLS12}
	if r.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(r.CACert)) {
			return nil, errors.New("the CA certificate is not PEM-encoded certificates")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// SystemCABundle is where the tools find the system's authorities when a TLS
// server has no CA of its own.
const SystemCABundle = "/etc/ssl/certs/ca-certificates.crt"

// NewRemoteMariaDB is the adapter of an external server. Nothing connects
// until first use.
func NewRemoteMariaDB(r RemoteServer, workDir string, e Exec) *MariaDBAdmin {
	return &MariaDBAdmin{Remote: &r, WorkDir: workDir, Tools: e}
}

// hostPart is the host an account is made for: the box's own accounts are
// 'localhost', an external server's '%' (the box connects from an address the
// server does not know in advance; TLS is what narrows it).
func (m *MariaDBAdmin) hostPart() string {
	if m.Remote != nil {
		return "%"
	}
	return "localhost"
}

// q is a statement as this server takes it: an account's host part
// 'localhost' read as '%', and, when the server insists on TLS, every
// account the agent creates or changes requires it. Statements are the
// agent's own constants, so the replacement never touches a value (values
// are bound).
func (m *MariaDBAdmin) q(query string) string {
	if m.Remote == nil {
		return query
	}
	query = strings.NewReplacer("'localhost'", "'%'", "`localhost`", "`%`").Replace(query)
	if m.Remote.TLS && (strings.HasPrefix(query, "CREATE USER") || strings.HasPrefix(query, "ALTER USER")) {
		query = strings.Replace(query, " WITH MAX_USER_CONNECTIONS", " REQUIRE SSL WITH MAX_USER_CONNECTIONS", 1)
	}
	return query
}

// rootConfig is the driver configuration of the administrator's pool.
func (m *MariaDBAdmin) rootConfig() (*mysql.Config, error) {
	if m.Remote == nil {
		return RootConfig(m.Socket), nil
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd = m.Remote.AdminUser, m.Remote.AdminPassword
	cfg.Net, cfg.Addr = "tcp", m.Remote.Addr()
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 5 * time.Minute
	cfg.WriteTimeout = 5 * time.Minute
	cfg.InterpolateParams = true
	cfg.AllowAllFiles = false
	if m.Remote.TLS {
		t, err := m.Remote.TLSConfig()
		if err != nil {
			return nil, err
		}
		cfg.TLS = t
	}
	return cfg, nil
}

// connLines are the [client] lines that say where the server is: the socket,
// or host, port and TLS (the CA is written beside the file, caFile).
func (m *MariaDBAdmin) connLines(caFile string) string {
	if m.Remote == nil {
		if m.Socket == "" {
			return ""
		}
		return "socket=" + OptionValue(m.Socket) + "\n"
	}
	b := "host=" + OptionValue(m.Remote.Host) + "\nport=" + strconv.Itoa(m.Remote.Port) + "\n"
	if m.Remote.TLS {
		ca := SystemCABundle
		if m.Remote.CACert != "" {
			ca = caFile
		}
		b += "ssl=1\nssl-ca=" + OptionValue(ca) + "\nssl-verify-server-cert=1\n"
	}
	return b
}

// protocolArg is the client's --protocol.
func (m *MariaDBAdmin) protocolArg() string {
	if m.Remote != nil {
		return "--protocol=tcp"
	}
	return "--protocol=socket"
}

// writeFile writes the client defaults file for an account of this server
// (and, for a TLS server with its own CA, the CA beside it: removeClientFile
// removes both).
func (m *MariaDBAdmin) writeFile(user, password string) (string, error) {
	return writeClientFile(m.WorkDir, m.connLines, m.Remote, user, password)
}

// removeClientFile removes a defaults file and the CA written beside it; a
// missing one is not an error.
func removeClientFile(file string) error {
	var errs []error
	for _, f := range []string{file, file + ".ca"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// adminClientFile is a defaults file signed in as the administrator of a
// remote server, for one import (the account has every privilege there; the
// client's --one-database keeps the SQL inside the database).
func (m *MariaDBAdmin) adminClientFile() (*ScopedAccount, error) {
	if m.Remote == nil {
		return nil, errors.New("not an external server")
	}
	if strings.ContainsAny(m.Remote.AdminPassword, "\x00\r\n") {
		return nil, errors.New("the administrator's password cannot go in a defaults file")
	}
	file, err := m.writeFile(m.Remote.AdminUser, OptionValue(m.Remote.AdminPassword))
	if err != nil {
		return nil, err
	}
	return &ScopedAccount{User: m.Remote.AdminUser, DefaultsFile: file, cleanup: func() error { return removeClientFile(file) }}, nil
}

// definerOf is the DEFINER every clause of an import is rewritten to on this
// server.
func (m *MariaDBAdmin) definerOf(as ImportAs) string {
	switch {
	case as.Login != nil:
		return "`" + as.Login.User + "`@`" + m.hostPart() + "`"
	case as.Site != "":
		return "`" + as.Site + "`@`" + m.hostPart() + "`"
	}
	return "CURRENT_USER"
}
