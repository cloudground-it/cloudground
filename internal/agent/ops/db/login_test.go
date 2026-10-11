package db

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/go-sql-driver/mysql"
)

// whoConn answers CURRENT_USER() with who.
type whoConn struct {
	who    string
	closed *int
}

func (c whoConn) ID() int64 { return 1 }
func (c whoConn) Query(context.Context, Limits, string, ...any) (rpc.DBQueryResult, error) {
	return rpc.DBQueryResult{}, nil
}
func (c whoConn) Each(_ context.Context, _ string, _ []any, fn func([]string, [][]byte) error) error {
	return fn(nil, [][]byte{[]byte(c.who)})
}
func (c whoConn) Exec(context.Context, string, ...any) (int64, error) { return 0, nil }
func (c whoConn) Close() error                                        { *c.closed++; return nil }

// A signed-in user's session (SPEC §3.1): the connection is the user's own,
// with no scoped account; a refused sign-in, a name that is not a site's
// and a server that matched another account are all ErrLoginRefused.
func TestLoginSession(t *testing.T) {
	var dialled []DialSpec
	closed := 0
	who := "shop_reader@localhost"
	var fail error
	o := &Ops{Dial: func(_ context.Context, d DialSpec) (Conn, error) {
		dialled = append(dialled, d)
		if fail != nil {
			return nil, fail
		}
		return whoConn{who: who, closed: &closed}, nil
	}}
	s, err := o.LoginSession(context.Background(), Login{User: "shop_reader", Password: "pw"}, "shop_data", 0)
	if err != nil {
		t.Fatal(err)
	}
	d := dialled[0]
	if d.Account != nil || d.Login.User != "shop_reader" || d.Login.Password != "pw" || d.Database != "shop_data" || d.StatementTimeout <= 0 {
		t.Fatalf("dialled %+v", d)
	}
	s.Close()
	if closed != 1 {
		t.Fatal("the connection was not closed")
	}
	cfg := scopedConfig("/run/mysqld/mysqld.sock", d)
	if cfg.User != "shop_reader" || cfg.Passwd != "pw" || cfg.AllowAllFiles || cfg.MultiStatements || cfg.Net != "unix" {
		t.Fatalf("driver config %+v", cfg)
	}

	who = "@localhost"
	if _, err := o.LoginSession(context.Background(), Login{User: "shop_reader", Password: "pw"}, "", 0); !errors.Is(err, ErrLoginRefused) {
		t.Fatalf("an anonymous match: %v", err)
	}
	who = "shop_reader@localhost"
	fail = &mysql.MySQLError{Number: 1045, Message: "Access denied"}
	if _, err := o.LoginSession(context.Background(), Login{User: "shop_reader", Password: "bad"}, "", 0); !errors.Is(err, ErrLoginRefused) {
		t.Fatalf("a wrong password: %v", err)
	}
	n := len(dialled)
	for _, l := range []Login{{User: "root", Password: "x"}, {User: "shop_reader"}, {User: "cgq_0123456789abcdef", Password: "x"}} {
		if _, err := o.LoginSession(context.Background(), l, "", 0); !errors.Is(err, ErrLoginRefused) {
			t.Fatalf("%+v: %v", l, err)
		}
	}
	if len(dialled) != n {
		t.Fatal("a name that is not a site's reached the server")
	}
	if _, err := o.LoginSession(context.Background(), Login{User: "shop_reader", Password: "pw"}, "mysql", 0); err == nil {
		t.Fatal("the server's own schema opened")
	}
}
