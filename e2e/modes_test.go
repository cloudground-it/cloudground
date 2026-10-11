//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/cloudground-it/cloudground/internal/phpfpm"
)

// The agent runs with UMask=0027: a directory it made with os.MkdirAll(p,
// 0755) came out 0750, and on a freshly imaged 26.04 every PHP site failed
// its FPM test because /etc/cloudground-php was closed to the site's user.
// These checks read the modes the product left on the box, and that the
// readers who need a path (the site's user, nginx's workers) really reach it.

// modeOwner is "<octal mode> <user>:<group>" of p, without following a
// symlink at its name, or the error.
func modeOwner(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return err.Error()
	}
	st := fi.Sys().(*syscall.Stat_t)
	name := func(id uint32, group bool) string {
		s := strconv.FormatUint(uint64(id), 10)
		if group {
			if g, err := user.LookupGroupId(s); err == nil {
				return g.Name
			}
			return s
		}
		if u, err := user.LookupId(s); err == nil {
			return u.Username
		}
		return s
	}
	return fmt.Sprintf("%o %s:%s", fi.Mode().Perm(), name(st.Uid, false), name(st.Gid, true))
}

// installedModes (R-6.1): every directory the installer makes for the
// sites' files, the PHP masters' configuration among them, is open to the
// workers and site users that traverse it.
func (h *harness) installedModes(t *testing.T) {
	for _, d := range []string{"/srv/sites", "/var/log/cloudground", "/var/cache/cloudground", "/var/www/cloudground-acme",
		"/run/cloudground", "/run/cloudground/php", "/etc/cloudground-php"} {
		h.check(t, "R-6.1: "+d+" is 0755 root:root", modeOwner(d), "755 root:root")
	}
}

// siteModes checks, right after a site provisioned, every directory the
// agent made for it, and for a PHP site that its user reads its pool and
// nginx reaches its socket.
func (h *harness) siteModes(t *testing.T, label, dom, u string, php bool) {
	root := "/srv/sites/" + dom
	h.check(t, label+": the site root is 0750 root:<site>", modeOwner(root), "750 root:"+u)
	h.check(t, label+": releases/ is 0750 root:<site>", modeOwner(root+"/releases"), "750 root:"+u)
	for _, d := range []string{"shared", "shared/uploads", "logs", "tmp", "tmp/sessions"} {
		h.check(t, label+": "+d+"/ is 0750 <site>:<site>", modeOwner(filepath.Join(root, d)), "750 "+u+":"+u)
	}
	h.check(t, label+": its log directory is 0750 root:<site>", modeOwner("/var/log/cloudground/"+dom), "750 root:"+u)
	if !php {
		return
	}
	h.check(t, label+": /etc/cloudground-php is 0755 root:root", modeOwner("/etc/cloudground-php"), "755 root:root")
	pool := "/etc/cloudground-php/" + u + ".conf"
	h.check(t, label+": the pool is 0640 root:<site>", modeOwner(pool), "640 root:"+u)
	r := h.run("runuser", "-u", u, "--", "head", "-c", "1", pool)
	h.assert(t, label+": the site's user reads its pool", r.OK() && r.Out == ";", "%s", r)
	h.check(t, label+": the slice drop-in's directory is 0755 root:root",
		modeOwner("/etc/systemd/system/"+phpfpm.UnitName(u)+".d"), "755 root:root")
	h.check(t, label+": php-error.log is 0640 <site>:<site>", modeOwner("/var/log/cloudground/"+dom+"/php-error.log"), "640 "+u+":"+u)
	sock := phpfpm.SocketFor(u)
	h.check(t, label+": the FPM socket's directory is 0750 <site>:<site>", modeOwner(filepath.Dir(sock)), "750 "+u+":"+u)
	// nginx's workers reach the socket through www-data's membership of the
	// site's group (the directory is 0750, the socket 0660 <site>:<site>).
	// "test -w" cannot prove it on 26.04: uutils' test compares the file's
	// group with the effective gid only, never the supplementary groups, so
	// it answers no (exit 1, silent) while nginx connects. connect(2) as
	// www-data, with the groups runuser gives it, is the real property.
	r = h.run("runuser", "-u", "www-data", "--", "python3", "-I", "-c", unixConnectProbe, sock)
	h.assert(t, label+": nginx's user can reach the FPM socket", r.OK(), "%s", r)
}

// unixConnectProbe connects to the unix socket argv[1] and closes it: exit
// 0 when the caller may connect, a traceback (EACCES, ENOENT) otherwise.
const unixConnectProbe = `import socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(5)
s.connect(sys.argv[1])
s.close()
`
