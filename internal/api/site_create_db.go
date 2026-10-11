package api

// The database a new site is made with, and what its creator is told once
// the site is created (SPEC.md §3.2, Fase 9b stream 2). WordPress,
// WooCommerce and Laravel always get one; a static or plain PHP site gets
// one when asked (create_database); a reverse proxy never does. The
// creator may choose the database's name, its user and the user's
// password; whatever is not chosen is the panel's (site_<id>, a generated
// password). The password is answered once, in the creation's response:
// no other answer ever carries it (a reset is the Database tab's, §6.5).

import (
	"errors"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The request fields a database problem names.
const (
	fieldCreateDB   = "create_database"
	fieldDBName     = "database_name"
	fieldDBUser     = "database_user"
	fieldDBPassword = "database_password"
)

// newSiteDB is the primary database a create asks for. Name and user are
// "" for the panel's own (site_<id>); password is "" for a generated one.
type newSiteDB struct {
	want                 bool
	name, user, password string
}

// fieldProblem is a refusal naming one field of the request.
func fieldProblem(kind problemKind, field, msg string) *problem {
	return &problem{kind: kind, msg: msg, fields: map[string]string{field: msg}}
}

// checkNewSiteDB reads the database fields of a create of a site of type
// kind: whether it gets a database, and the names and password its creator
// chose, checked (internal/site rules; a site_<n> name is the panel's and
// never chosen: the site has no number yet).
func checkNewSiteDB(req *newSite, kind state.SiteType) (newSiteDB, error) {
	var d newSiteDB
	switch {
	case startsWithDB(kind):
		if req.CreateDatabase != nil && !*req.CreateDatabase {
			return d, fieldProblem(probInvalid, fieldCreateDB, "WordPress, WooCommerce and Laravel sites always have a database")
		}
		d.want = true
	case kind == state.SiteProxy:
		if req.CreateDatabase != nil && *req.CreateDatabase {
			return d, fieldProblem(probInvalid, fieldCreateDB, "a reverse proxy site has no database")
		}
	default:
		d.want = req.CreateDatabase != nil && *req.CreateDatabase
	}
	d.name = strings.TrimSpace(req.DatabaseName)
	d.user = strings.TrimSpace(req.DatabaseUser)
	d.password = req.DatabasePassword
	if !d.want {
		for field, v := range map[string]string{fieldDBName: d.name, fieldDBUser: d.user, fieldDBPassword: d.password} {
			if v != "" {
				return d, fieldProblem(probInvalid, field, "this site gets no database: set create_database to make one")
			}
		}
		return d, nil
	}
	if d.name != "" {
		if err := site.ClaimableBy(d.name, 0); err != nil {
			return d, fieldProblem(probInvalid, fieldDBName, "the database name: "+err.Error())
		}
	}
	if d.user != "" {
		if err := site.ClaimableBy(d.user, 0); err != nil {
			return d, fieldProblem(probInvalid, fieldDBUser, "the database user: "+err.Error())
		}
	}
	if d.password != "" {
		if msg := dbPasswordProblem(d.password); msg != "" {
			return d, fieldProblem(probInvalid, fieldDBPassword, msg)
		}
	}
	return d, nil
}

// claimFree refuses, before anything is written, a chosen name some site
// already has: 409 naming the field. The insert of the rows checks again.
func (svc siteService) claimFree(d newSiteDB) error {
	st := svc.srv.Store
	if d.name != "" {
		taken, err := st.DBNameInUse(d.name, false)
		if err != nil {
			return err
		}
		if taken {
			return fieldProblem(probConflict, fieldDBName, "a database called "+d.name+" already exists on this server")
		}
	}
	if d.user != "" {
		taken, err := st.DBNameInUse(d.user, true)
		if err != nil {
			return err
		}
		if taken {
			return fieldProblem(probConflict, fieldDBUser, "a database user called "+d.user+" already exists on this server")
		}
	}
	return nil
}

// primaryConfig is the site's config.database for d: the chosen names or
// the panel's, on the local server.
func (d newSiteDB) primaryConfig(id int64) state.DatabaseConfig {
	name, user := d.name, d.user
	if name == "" {
		name = siteDatabase(id)
	}
	if user == "" {
		user = siteDatabase(id)
	}
	return state.DatabaseConfig{Enabled: true, Host: "127.0.0.1", Port: 3306, Name: name, User: user}
}

// errPrimaryTaken is recordPrimaryDB's answer for a name another site took
// between claimFree and the insert; field says which.
type errPrimaryTaken struct{ field, name string }

func (e errPrimaryTaken) Error() string { return e.name + " is already in use on this server" }
func (e errPrimaryTaken) Unwrap() error { return state.ErrDBNameTaken }

// takenProblem is the 409 for a name recordPrimaryDB found taken.
func takenProblem(err error) *problem {
	var pt errPrimaryTaken
	if !errors.As(err, &pt) {
		return refuse(probConflict, "the site's database name is already in use on this server")
	}
	what := "database"
	if pt.field == fieldDBUser {
		what = "database user"
	}
	return fieldProblem(probConflict, pt.field, "a "+what+" called "+pt.name+" already exists on this server")
}

// siteSummary is what the creator of a site needs to use it, answered once
// with the creation (POST /api/sites "summary"). Database.Password is the
// only place the panel ever answers the primary user's password.
type siteSummary struct {
	SiteURL   string            `json:"site_url"`
	PanelPath string            `json:"panel_path"`
	PanelURL  string            `json:"panel_url,omitempty"`
	WordPress *wordpressSummary `json:"wordpress,omitempty"`
	Database  *databaseSummary  `json:"database,omitempty"`
	SFTP      sftpSummary       `json:"sftp"`
}

type wordpressSummary struct {
	AdminURL  string `json:"admin_url"`
	AdminUser string `json:"admin_user"`
}

type databaseSummary struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Socket   string `json:"socket,omitempty"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
}

// sftpSummary is the site's SFTP login: a new site's is off until it is
// turned on (the SFTP tab), so Host and Port are set only when it is on.
type sftpSummary struct {
	Enabled bool   `json:"enabled"`
	User    string `json:"user"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
}

// summaryOf builds the summary of a site just created. host is the
// address the panel was reached at (for the panel link and SFTP), "" when
// it is not one a person can open (the API's Unix socket).
func summaryOf(rec state.Site, adminUser, dbPassword, socket, panelHost string) siteSummary {
	out := siteSummary{
		SiteURL:   "https://" + rec.Domain + "/",
		PanelPath: "/sites/" + strconv.FormatInt(rec.ID, 10),
		SFTP:      sftpSummary{Enabled: rec.Config.SFTPEnabled, User: rec.SystemUser},
	}
	if panelHost != "" {
		out.PanelURL = "https://" + panelHost + out.PanelPath
	}
	if isWordPress(rec.Type) {
		out.WordPress = &wordpressSummary{AdminURL: "https://" + rec.Domain + "/wp-admin/", AdminUser: adminUser}
	}
	if localDB(rec) {
		d := rec.Config.Database
		out.Database = &databaseSummary{Host: d.Host, Port: d.Port, Socket: socket, Name: d.Name, User: d.User, Password: dbPassword}
	}
	if out.SFTP.Enabled && panelHost != "" {
		h := panelHost
		if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.HasSuffix(h, "]") {
			h = h[:i]
		}
		out.SFTP.Host, out.SFTP.Port = h, 22
	}
	return out
}

// panelHost is the address a person reaches the panel at: the request's
// Host when it is an address, else (a request on the API's Unix socket,
// cgctl's, names none) the domain the panel holds a certificate for, on
// the panel's port (8443, ops/system SPEC); "" when there is neither.
func panelHost(ps state.PanelSettings, reqHost string) string {
	if strings.ContainsAny(reqHost, ".:") {
		return reqHost
	}
	if ps.PanelDomain != "" {
		return ps.PanelDomain + ":8443"
	}
	return ""
}
