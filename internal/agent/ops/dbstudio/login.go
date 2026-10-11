package dbstudio

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/db"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Signed-in database users (SPEC §11): a person signs in to Quarry with
// a database user's own name and password, and every statement of that
// session runs as that user, so what it sees and changes is what the server
// grants it. The agent keeps the password for the session's life only, in
// memory, sealed with a key made at start and never written anywhere.

// Limits of the pool of signed-in sessions (SPEC §11).
const (
	// loginIdle ends a session nobody used for this long: a little more
	// than the API's own idle time, which ends it first and says so.
	loginIdle = 35 * time.Minute
	// loginLife ends any session this long after it started.
	loginLife = 12*time.Hour + 5*time.Minute
	// maxLogins is how many sessions the agent keeps; a new one past it
	// ends the one unused the longest.
	maxLogins = 256
	// maxSiteLogins is the same per site.
	maxSiteLogins = 32
)

// errLoginGone is a session the agent no longer has (ended, expired, or
// the agent restarted) or whose password the server stopped accepting.
func errLoginGone(msg string) error {
	return &agentrpc.Error{Code: agentrpc.CodeUnauthorized, Message: msg}
}

// login is one signed-in session.
type login struct {
	site          int64
	user          string
	nonce, sealed []byte
	started, used time.Time
}

// logins is the pool, keyed by handle. The zero value is ready.
type logins struct {
	once sync.Once
	aead cipher.AEAD
	err  error

	mu   sync.Mutex
	byID map[string]*login
}

func (l *logins) init() error {
	l.once.Do(func() {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			l.err = err
			return
		}
		block, err := aes.NewCipher(key)
		if err == nil {
			l.aead, err = cipher.NewGCM(block)
		}
		l.err = err
		l.byID = map[string]*login{}
	})
	return l.err
}

// expired reports whether a session is past its idle time or its life.
func (in *login) expired(at time.Time) bool {
	return at.Sub(in.used) > loginIdle || at.Sub(in.started) > loginLife
}

// add keeps a new session and returns its handle.
func (l *logins) add(site int64, user, password string, at time.Time) (string, error) {
	if err := l.init(); err != nil {
		return "", err
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	handle := hex.EncodeToString(id)
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	in := &login{site: site, user: user, nonce: nonce, started: at, used: at,
		sealed: l.aead.Seal(nil, nonce, []byte(password), []byte(handle))}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(at)
	l.evict(func(x *login) bool { return x.site == site }, maxSiteLogins-1)
	l.evict(func(*login) bool { return true }, maxLogins-1)
	l.byID[handle] = in
	return handle, nil
}

// evict ends the least recently used sessions that match until at most
// keep are left. Callers hold mu.
func (l *logins) evict(match func(*login) bool, keep int) {
	var ids []string
	for id, in := range l.byID {
		if match(in) {
			ids = append(ids, id)
		}
	}
	if len(ids) <= keep {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return l.byID[ids[i]].used.Before(l.byID[ids[j]].used) })
	for _, id := range ids[:len(ids)-keep] {
		delete(l.byID, id)
	}
}

// sweep forgets the expired sessions. Callers hold mu.
func (l *logins) sweep(at time.Time) {
	for id, in := range l.byID {
		if in.expired(at) {
			delete(l.byID, id)
		}
	}
}

// get returns the session's user and password, counting a use; a session
// of another site is as unknown as a missing one.
func (l *logins) get(site int64, handle string, at time.Time) (db.Login, error) {
	if err := l.init(); err != nil {
		return db.Login{}, err
	}
	l.mu.Lock()
	in, ok := l.byID[handle]
	if ok && in.expired(at) {
		delete(l.byID, handle)
		ok = false
	}
	if !ok || in.site != site {
		l.mu.Unlock()
		return db.Login{}, errLoginGone("the Quarry session has ended: sign in again")
	}
	in.used = at
	user, nonce, sealed := in.user, in.nonce, in.sealed
	l.mu.Unlock()
	pw, err := l.aead.Open(nil, nonce, sealed, []byte(handle))
	if err != nil {
		return db.Login{}, errLoginGone("the Quarry session has ended: sign in again")
	}
	return db.Login{User: user, Password: string(pw)}, nil
}

// drop ends a session of the site.
func (l *logins) drop(site int64, handle string) {
	if l.init() != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if in, ok := l.byID[handle]; ok && in.site == site {
		delete(l.byID, handle)
	}
}

// count is how many sessions the pool holds (tests).
func (l *logins) count() int {
	if l.init() != nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byID)
}

// SignIn signs one of the site's database users in: the user must be one the
// record gives the site, and the server must take its password (SPEC §11).
// A refusal says nothing more than that, whatever the reason.
func (o *Ops) SignIn(ctx context.Context, p rpc.DBStudioOpenParams) (rpc.DBStudioOpenResult, error) {
	refused := rpc.DBStudioOpenResult{Refused: true}
	if err := o.DB.SiteUser(ctx, p.Site, p.User); err != nil {
		if agentrpc.CodeOf(err) == agentrpc.CodeInvalid {
			return refused, nil
		}
		return rpc.DBStudioOpenResult{}, err
	}
	login := db.Login{User: p.User, Password: p.Password}
	s, err := o.DB.LoginSession(ctx, login, "", browseTimeout)
	if errors.Is(err, db.ErrLoginRefused) {
		return refused, nil
	}
	if err != nil {
		return rpc.DBStudioOpenResult{}, err
	}
	dbs, err := databases(ctx, s)
	s.Close()
	if err != nil {
		return rpc.DBStudioOpenResult{}, err
	}
	handle, err := o.logins.add(p.Site.ID, p.User, p.Password, o.now())
	if err != nil {
		return rpc.DBStudioOpenResult{}, err
	}
	return rpc.DBStudioOpenResult{Login: handle, Databases: dbs}, nil
}

// SignOut ends a signed-in session. Ending one already gone is not an error.
func (o *Ops) SignOut(_ context.Context, p rpc.DBStudioLoginParams) (agentrpc.None, error) {
	o.logins.drop(p.Site.ID, p.Login)
	return agentrpc.None{}, nil
}

// Databases lists the databases a signed-in user may see, now.
func (o *Ops) Databases(ctx context.Context, p rpc.DBStudioLoginParams) (rpc.DBStudioDatabasesResult, error) {
	s, err := o.loginSession(ctx, p.Site, p.Login, "", browseTimeout)
	if err != nil {
		return rpc.DBStudioDatabasesResult{}, err
	}
	defer s.Close()
	dbs, err := databases(ctx, s)
	return rpc.DBStudioDatabasesResult{Databases: dbs}, err
}

// databases is SHOW DATABASES as the session's user, without the server's
// own schemas (Quarry does not browse them).
func databases(ctx context.Context, s *db.Session) ([]string, error) {
	rows, err := collect(ctx, s, 10000, "SHOW DATABASES")
	if err != nil {
		return nil, sqlErr(s, err)
	}
	out := []string{}
	for _, r := range rows {
		if name := str(r[0]); host.ValidDatabaseName(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loginSession opens a connection as a signed-in session's user on
// database. A password the server no longer takes (it was changed, the
// user dropped) ends the session.
func (o *Ops) loginSession(ctx context.Context, s state.Site, handle, database string, timeout time.Duration) (*db.Session, error) {
	login, err := o.logins.get(s.ID, handle, o.now())
	if err != nil {
		return nil, err
	}
	sess, err := o.DB.LoginSession(ctx, login, database, timeout)
	if errors.Is(err, db.ErrLoginRefused) {
		if database != "" {
			// The password may still be right: the user may only have lost
			// this database. Ask again without it.
			if probe, perr := o.DB.LoginSession(ctx, login, "", timeout); perr == nil {
				probe.Close()
				return nil, agentrpc.Invalid("%s has no access to the database %s", login.User, database)
			}
		}
		o.logins.drop(s.ID, handle)
		return nil, errLoginGone("the database no longer accepts this user's name and password: sign in again")
	}
	return sess, err
}

// sessionFor is where every Quarry statement runs (SPEC §2, §11): as
// the signed-in user when handle is set (the server's grants decide, and
// readOnly is not a grant it can add), otherwise as a scoped account on
// one of the site's databases, read-only when asked.
func (o *Ops) sessionFor(ctx context.Context, s state.Site, handle, database string, readOnly bool, timeout time.Duration) (*db.Session, error) {
	if handle == "" {
		return o.session(ctx, s, database, readOnly, timeout)
	}
	if database == "" {
		return nil, agentrpc.Invalid("choose a database")
	}
	return o.loginSession(ctx, s, handle, database, timeout)
}

// credentials returns a signed-in session's user and password, for what
// runs a client (a dump, an import) as that user.
func (o *Ops) credentials(s state.Site, handle string) (db.Login, error) {
	return o.logins.get(s.ID, handle, o.now())
}
