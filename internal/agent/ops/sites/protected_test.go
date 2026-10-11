package sites

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #26: each protected path gets its own password file, root's and nginx's
// group's only, named after the path's digest; the vhost names it; a path
// no longer protected loses its file at the next render, and a deleted
// site loses them all.
func TestProtectedPathFiles(t *testing.T) {
	e := newEnv(t)
	hash := "$2a$10$" + strings.Repeat("c", 53)
	s := e.site(31, "locked.example.com", state.SiteWordPress)
	s.Config.ProtectedPaths = []state.ProtectedPath{
		{Path: "/wp-admin", Username: "boss", PasswordHash: hash},
		{Path: "/private", Username: "team", PasswordHash: hash},
	}
	e.create(t, s)
	admin := protectedAuthFile(e.Paths.NginxAuthDir, s.Domain, "/wp-admin")
	private := protectedAuthFile(e.Paths.NginxAuthDir, s.Domain, "/private")
	if b, err := os.ReadFile(admin); err != nil || string(b) != "boss:"+hash+"\n" {
		t.Fatalf("password file %s = %q, %v", admin, b, err)
	}
	if fi, err := os.Stat(private); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("password file mode = %v, %v", fi, err)
	}
	vhost, _ := os.ReadFile(e.vhostPath(s))
	if !strings.Contains(string(vhost), admin) || !strings.Contains(string(vhost), "auth_basic $cg_auth_realm_31;") {
		t.Fatalf("the vhost does not name the password file:\n%s", vhost)
	}

	s.Config.ProtectedPaths = s.Config.ProtectedPaths[:1]
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if exists(private) || !exists(admin) {
		t.Fatalf("after unprotecting /private: private file %v, admin file %v", exists(private), exists(admin))
	}

	// A hash that is not bcrypt never reaches a password file.
	bad := s
	bad.Config.ProtectedPaths = []state.ProtectedPath{{Path: "/wp-admin", Username: "boss", PasswordHash: "plain"}}
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: bad}); err == nil {
		t.Fatal("a non-bcrypt hash was rendered")
	}
	if b, _ := os.ReadFile(admin); string(b) != "boss:"+hash+"\n" {
		t.Fatalf("a refused render changed the password file: %q", b)
	}

	if _, err := e.Delete(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(e.Paths.NginxAuthDir, s.Domain+".path-*")); len(left) != 0 {
		t.Fatalf("password files outlived the site: %v", left)
	}
}
