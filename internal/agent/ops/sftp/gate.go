package sftp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/siteshell"
	"github.com/cloudground-it/cloudground/internal/state"
)

// The gate's limits (SPEC §5.4).
const (
	// gateMaxConns bounds the connections the gate holds at once, sessions
	// included; one past it is closed unanswered.
	gateMaxConns = 64
	// gateMaxPerUID bounds the open sessions of one site.
	gateMaxPerUID = 8
	// gateRate is how many sessions one site may start per gateRateWindow.
	// Tools open one session per command (Ansible, Deployer without
	// multiplexing, an IDE's remote helpers, a deploy script's ssh calls),
	// so a minute of ordinary use reaches tens of them: 20 refused a single
	// deploy run. Two a second still bounds the transient units a site can
	// make PID 1 create; the open sessions stay capped by gateMaxPerUID.
	gateRate       = 120
	gateRateWindow = time.Minute
	// gateRequestTimeout bounds the wait for the request line.
	gateRequestTimeout = 2 * time.Second
	// maxRequest bounds the request line (the command included).
	maxRequest = 64 << 10
	// stopTimeout bounds the stop of a session's unit.
	stopTimeout = 30 * time.Second
)

// gateLimits counts the gate's connections and each uid's sessions.
type gateLimits struct {
	mu      sync.Mutex
	conns   int
	perUID  map[uint32]int
	started map[uint32][]time.Time
	now     func() time.Time
}

// enter admits one more connection.
func (g *gateLimits) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns >= gateMaxConns {
		return false
	}
	g.conns++
	return true
}

func (g *gateLimits) leave() {
	g.mu.Lock()
	g.conns--
	g.mu.Unlock()
}

// session admits a session of uid and returns its release.
func (g *gateLimits) session(uid uint32) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.perUID == nil {
		g.perUID, g.started = map[uint32]int{}, map[uint32][]time.Time{}
	}
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	if g.perUID[uid] >= gateMaxPerUID {
		return nil, fmt.Errorf("too many open shells for this site (at most %d)", gateMaxPerUID)
	}
	recent := g.started[uid][:0]
	for _, t := range g.started[uid] {
		if now.Sub(t) < gateRateWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= gateRate {
		g.started[uid] = recent
		return nil, errors.New("too many shells started for this site; try again in a minute")
	}
	g.started[uid] = append(recent, now)
	g.perUID[uid]++
	return func() {
		g.mu.Lock()
		g.perUID[uid]--
		g.mu.Unlock()
	}, nil
}

func (o *Ops) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

// ServeGate listens on the gate socket until ctx ends. The socket is
// root:cg-shell 0660: only shell logins reach it, never PHP or the shell's
// own processes (they run as the site user, who is not in cg-shell).
func (o *Ops) ServeGate(ctx context.Context) error {
	if err := o.shellReady(); err != nil {
		return err
	}
	if err := o.Accounts.EnsureGroup(ctx, ShellGroup, true); err != nil {
		return fmt.Errorf("the shell group: %w", err)
	}
	// An earlier build listed the site users in cron.deny, which broke
	// root's crontab -u for them (SPEC §5.7).
	if err := o.RemoveDenyBlocks(); err != nil {
		o.log().Warn("removing the old cron.deny and at.deny block", "err", err)
	}
	lookup := o.LookupGID
	if lookup == nil {
		lookup = func(name string) (int, error) {
			g, err := osuser.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(g.Gid)
		}
	}
	gid, err := lookup(ShellGroup)
	if err != nil {
		return fmt.Errorf("the shell group: %w", err)
	}
	path := o.ShellPaths.Socket
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("the shell gate: %w", err)
	}
	defer l.Close()
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !o.gate.enter() {
			c.Close()
			continue
		}
		go func() {
			defer o.gate.leave()
			o.serveGateConn(c)
		}()
	}
}

// gateSession is one admitted request.
type gateSession struct {
	req      siteshell.Request
	stdio    [3]*os.File
	siteUser string
	site     state.Site
	tty      string // /dev/pts/<n> when stdin is the session's terminal
	unit     string
}

func (o *Ops) serveGateConn(c *net.UnixConn) {
	defer c.Close()
	answer := func(format string, a ...any) {
		_, _ = c.Write([]byte(strings.ReplaceAll(fmt.Sprintf(format, a...), "\n", " ") + "\n"))
	}
	_ = c.SetReadDeadline(time.Now().Add(gateRequestTimeout))
	req, stdio, rest, err := readRequest(c)
	defer func() {
		for _, f := range stdio {
			if f != nil {
				f.Close()
			}
		}
	}()
	if err != nil {
		answer("error %v", err)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	cred, err := peerCred(c)
	if err != nil {
		answer("error no peer credentials")
		return
	}
	sess, release, err := o.admit(cred, req, stdio)
	if err != nil {
		o.log().Warn("shell gate: refused", "pid", cred.Pid, "uid", cred.Uid, "err", err)
		answer("error %v", err)
		return
	}
	defer release()
	argv := o.shellArgv(sess)
	var spawnIO [3]*os.File
	if sess.tty == "" {
		spawnIO = stdio
	}
	spawn := o.Spawn
	if spawn == nil {
		spawn = spawnReal
	}
	wait, err := spawn(argv, spawnIO)
	if err != nil {
		answer("error the shell could not start")
		o.log().Warn("shell gate: systemd-run", "err", err)
		return
	}
	o.log().Info("site shell session", "user", sess.siteUser, "pid", cred.Pid, "unit", sess.unit, "tty", sess.tty != "")
	answer("ok %s", sess.unit)
	// The client asks for a stop (it was hung up on or killed) or goes
	// away: the session's unit is stopped.
	done := make(chan struct{})
	go func() {
		r := bufio.NewReader(io2(rest, c))
		_, _ = r.ReadString('\n')
		select {
		case <-done:
		default:
			ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			_ = o.Systemd.Stop(ctx, sess.unit)
			cancel()
		}
	}()
	code := wait()
	close(done)
	answer("exit %d", code)
}

// io2 reads what followed the request in the first read, then the
// connection.
func io2(rest []byte, c *net.UnixConn) *multiReader {
	return &multiReader{rest: rest, c: c}
}

type multiReader struct {
	rest []byte
	c    *net.UnixConn
}

func (m *multiReader) Read(p []byte) (int, error) {
	if len(m.rest) > 0 {
		n := copy(p, m.rest)
		m.rest = m.rest[n:]
		return n, nil
	}
	return m.c.Read(p)
}

// readRequest reads the request line and the three descriptors that come
// with it (SCM_RIGHTS on its first bytes). It returns what was read past
// the line.
func readRequest(c *net.UnixConn) (siteshell.Request, [3]*os.File, []byte, error) {
	var stdio [3]*os.File
	var req siteshell.Request
	buf := make([]byte, maxRequest)
	oob := make([]byte, unix.CmsgSpace(4*4))
	n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return req, stdio, nil, errors.New("no request")
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return req, stdio, nil, errors.New("too many descriptors")
	}
	var fds []int
	if oobn > 0 {
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return req, stdio, nil, errors.New("bad descriptors")
		}
		for _, m := range msgs {
			got, err := unix.ParseUnixRights(&m)
			if err != nil {
				continue
			}
			fds = append(fds, got...)
		}
	}
	for i, fd := range fds {
		if i < 3 {
			stdio[i] = os.NewFile(uintptr(fd), "stdio"+strconv.Itoa(i))
		} else {
			unix.Close(fd)
		}
	}
	if len(fds) != 3 {
		return req, stdio, nil, errors.New("the request needs stdin, stdout and stderr")
	}
	data := buf[:n]
	for !bytes.Contains(data, []byte("\n")) {
		if len(data) >= maxRequest {
			return req, stdio, nil, errors.New("request too long")
		}
		m, err := c.Read(buf[len(data):])
		if err != nil {
			return req, stdio, nil, errors.New("no request")
		}
		data = buf[:len(data)+m]
	}
	i := bytes.IndexByte(data, '\n')
	if err := json.Unmarshal(data[:i], &req); err != nil {
		return req, stdio, nil, errors.New("bad request")
	}
	return req, stdio, append([]byte(nil), data[i+1:]...), nil
}

func peerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

// admit checks a request: a shell login of exactly one site, within the
// limits, with a valid command and environment, and finds its terminal.
func (o *Ops) admit(cred *unix.Ucred, req siteshell.Request, stdio [3]*os.File) (*gateSession, func(), error) {
	if cred.Pid <= 1 {
		return nil, nil, errors.New("not a site shell")
	}
	if err := req.Check(); err != nil {
		return nil, nil, err
	}
	siteUser, home, err := o.siteOfUID(int(cred.Uid))
	if err != nil {
		return nil, nil, err
	}
	s, err := o.siteOfHome(home)
	if err != nil {
		return nil, nil, err
	}
	s.SystemUser = siteUser
	release, err := o.gate.session(cred.Uid)
	if err != nil {
		return nil, nil, err
	}
	sess := &gateSession{req: req, stdio: stdio, siteUser: siteUser, site: s,
		unit: fmt.Sprintf("%s-%d.service", site.ShellUser(siteUser), cred.Pid)}
	sess.tty = ttyOf(stdio[0], cred.Uid)
	return sess, release, nil
}

// siteOfHome is the site whose root is home: a folder directly under
// SitesRoot named after a domain.
func (o *Ops) siteOfHome(home string) (state.Site, error) {
	root := o.ShellPaths.SitesRoot
	if root == "" {
		root = "/srv/sites"
	}
	if filepath.Clean(home) != home || filepath.Dir(home) != filepath.Clean(root) {
		return state.Site{}, errors.New("the site user's home is not a site")
	}
	domain := filepath.Base(home)
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") {
		return state.Site{}, errors.New("the site user's home is not a site")
	}
	return state.Site{Domain: domain, RootPath: home}, nil
}

// ttyOf is /dev/pts/<n> when f is a pseudo-terminal slave of uid that is
// that very device; "" otherwise (a pipe, a socket, someone else's tty).
func ttyOf(f *os.File, uid uint32) string {
	if f == nil {
		return ""
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Uid != uid {
		return ""
	}
	major, minor := unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev))
	if major < 136 || major > 143 {
		return ""
	}
	path := "/dev/pts/" + strconv.FormatUint(uint64((major-136)*256+minor), 10)
	var pst unix.Stat_t
	if err := unix.Stat(path, &pst); err != nil || pst.Rdev != st.Rdev {
		return ""
	}
	return path
}

// shellArgv is the systemd-run command line of a session: a transient
// service in the site's slice, as the site user, in the PHP master's
// sandbox (phpfpm.Sandbox), on the session's terminal or its descriptors.
func (o *Ops) shellArgv(sess *gateSession) []string {
	s := sess.site
	wd := s.RootPath
	if sess.req.Interactive() {
		if fi, err := os.Stat(filepath.Join(s.RootPath, "current")); err == nil && fi.IsDir() {
			wd = filepath.Join(s.RootPath, "current")
		}
	}
	argv := []string{"/bin/bash", "-c", sess.req.Command}
	if sess.req.Interactive() {
		argv = []string{"/bin/bash", "-l"}
	}
	return append([]string{"systemd-run"}, host.ServiceArgs(host.Service{
		Unit: sess.unit, Description: "SSH shell of " + s.Domain,
		User: sess.siteUser, Slice: site.Slice(sess.siteUser), Dir: wd,
		Env:   shellEnv(s, sess.siteUser, wd, sess.req),
		Props: append(phpfpm.RunSandbox(s), ShellQuiet...),
		TTY:   sess.tty, Pipe: sess.tty == "",
		Argv: argv,
	})...)
}

// ShellQuiet hides from the shell the login scripts that read what the
// sandbox does not show. systemd's terminal integration
// (/etc/profile.d/80-systemd-osc-context.sh, Ubuntu 26.04) reads
// /proc/sys/kernel/random/boot_id and .../uuid, which ProcSubset=pid
// leaves out of /proc, so every interactive login printed two "No such
// file or directory" lines. /etc/profile sources only the profile.d files
// it can read: an inaccessible one is skipped in silence. The "-" makes a
// system without the file fine. Nothing is opened up: the file only sends
// a terminal its title sequences.
var ShellQuiet = []string{"InaccessiblePaths=-/etc/profile.d/80-systemd-osc-context.sh"}

// ShellHardening is what the shell adds to the PHP master's sandbox: the
// one list every run off the request path takes (phpfpm.RunHardening).
var ShellHardening = phpfpm.RunHardening

// shellEnv is the session's environment: the site's, plus the terminal
// and language variables the client passed (checked by Request.Check).
func shellEnv(s state.Site, siteUser, wd string, req siteshell.Request) []string {
	env := []string{
		"HOME=" + s.RootPath, "USER=" + siteUser, "LOGNAME=" + siteUser, "SHELL=/bin/bash",
		"PATH=/usr/local/bin:/usr/bin:/bin", "PWD=" + wd,
		// The site root is root's; /tmp is the session's own (PrivateTmp).
		"TMPDIR=/tmp", "XDG_CACHE_HOME=/tmp/.cache", "XDG_CONFIG_HOME=/tmp/.config",
		"COMPOSER_HOME=/tmp/.composer", "WP_CLI_CACHE_DIR=/tmp/.wp-cli-cache",
	}
	term := req.Term
	if term == "" {
		term = "dumb"
	}
	env = append(env, "TERM="+term)
	return append(env, req.Env...)
}

// spawnReal starts systemd-run with stdio (nil: /dev/null) and an
// environment of its own, nothing of the agent's.
func spawnReal(argv []string, stdio [3]*os.File) (func() int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio[0], stdio[1], stdio[2]
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() int {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee) && ee.ExitCode() >= 0:
			return ee.ExitCode()
		}
		return 255
	}, nil
}

// pwEntry is one line of the user database.
type pwEntry struct {
	name     string
	uid, gid int
	home     string
}

func (o *Ops) passwd() ([]pwEntry, error) {
	p := o.ShellPaths.Passwd
	if p == "" {
		p = "/etc/passwd"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var out []pwEntry
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, pwEntry{name: f[0], uid: uid, gid: gid, home: f[5]})
	}
	return out, nil
}

// passwdUID is name's uid in the user database.
func (o *Ops) passwdUID(name string) (int, bool, error) {
	ents, err := o.passwd()
	if err != nil {
		return 0, false, fmt.Errorf("read the user database: %w", err)
	}
	for _, e := range ents {
		if e.name == name {
			return e.uid, true, nil
		}
	}
	return 0, false, nil
}

// siteOfUID is the site user whose shell login has uid, and its home:
// exactly one site user and its shell login must have it.
func (o *Ops) siteOfUID(uid int) (string, string, error) {
	ents, err := o.passwd()
	if err != nil {
		return "", "", fmt.Errorf("read the user database: %w", err)
	}
	var sites []pwEntry
	shells := map[string]bool{}
	for _, e := range ents {
		if e.uid != uid {
			continue
		}
		if site.ValidSystemUser(e.name) {
			sites = append(sites, e)
		}
		if s := site.SiteUserOfShell(e.name); s != "" {
			shells[s] = true
		}
	}
	if len(sites) != 1 || !shells[sites[0].name] {
		return "", "", errors.New("no site shell for this user")
	}
	return sites[0].name, sites[0].home, nil
}
