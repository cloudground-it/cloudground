package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// System returns the Env of this machine: commands, apt, systemctl, the
// accounts in /etc/passwd and /etc/group, HTTPS downloads. log receives the
// commands and their output.
func System(log io.Writer) *Env {
	if log == nil {
		log = io.Discard
	}
	c := &osCommands{log: log}
	return &Env{
		Root:        "/",
		Cmd:         c,
		Pkg:         &apt{c},
		Sd:          &systemctl{c},
		Acct:        &osAccounts{c},
		FS:          osFiles{},
		Net:         httpsFetcher{client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: httpsOnly}},
		Log:         log,
		Sleep:       time.Sleep,
		EUID:        os.Geteuid(),
		Arch:        unameArch(),
		FreeMB:      freeMB,
		Listening:   func(port int) bool { return listening("/", port) },
		PanelHealth: func(ctx context.Context) error { return panelHealth(ctx, APISocket) },
	}
}

// APISocket is where the API listens (cloudground-api.socket).
const APISocket = "/run/cloudground/api.sock"

// panelHealth is GET /healthz over the API's Unix socket: nil on 200, which
// the API answers only when it reaches the agent.
func panelHealth(ctx context.Context, sock string) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: the API does not reach the agent", resp.StatusCode)
	}
	return nil
}

func unameArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

type osCommands struct{ log io.Writer }

func (c *osCommands) Run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	fmt.Fprintf(c.log, "$ %s %s\n", name, strings.Join(args, " "))
	err := cmd.Run()
	_, _ = c.log.Write(out.Bytes())
	if err != nil {
		fmt.Fprintf(c.log, "(%v)\n", err)
	}
	return out.String(), err
}

// Quiet runs a command whose output must not reach the install log (the
// setup link is a credential): the log gets the command line only.
func (c *osCommands) Quiet(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	fmt.Fprintf(c.log, "$ %s %s (output not logged)\n", name, strings.Join(args, " "))
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func (c *osCommands) LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// apt is the package manager.
type apt struct{ c Commands }

func (a *apt) Missing(ctx context.Context, pkgs []string) ([]string, error) {
	var missing []string
	for _, p := range pkgs {
		out, err := a.c.Run(ctx, "dpkg-query", "-W", "-f=${Status}", p)
		// "hold ok installed": the operator pinned it; it is installed.
		if st := strings.TrimSpace(out); err != nil || (st != "install ok installed" && st != "hold ok installed") {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

func (a *apt) Update(ctx context.Context) error {
	_, err := a.c.Run(ctx, "apt-get", "update", "-qq")
	return err
}

func (a *apt) Install(ctx context.Context, pkgs []string) error {
	if len(pkgs) == 0 {
		return nil
	}
	if _, err := a.c.Run(ctx, "apt-get", append([]string{"install", "-y", "-qq"}, pkgs...)...); err != nil {
		return fmt.Errorf("apt-get install %s: %w", strings.Join(pkgs, " "), err)
	}
	return nil
}

func (a *apt) AddPPA(ctx context.Context, ppa string) error {
	_, err := a.c.Run(ctx, "add-apt-repository", "-y", "ppa:"+ppa)
	return err
}

// systemctl is systemd, through its command line.
type systemctl struct{ c Commands }

func (s *systemctl) Running() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

func (s *systemctl) Exists(ctx context.Context, unit string) (bool, error) {
	out, _ := s.c.Run(ctx, "systemctl", "list-unit-files", "--no-legend", "--full", unit)
	return strings.Contains(out, unit), nil
}

// state runs is-enabled or is-active, whose exit status is not an error
// but the answer; only a systemctl that printed nothing failed to answer.
func (s *systemctl) state(ctx context.Context, verb, unit string) (string, error) {
	out, err := s.c.Run(ctx, "systemctl", verb, unit)
	out = strings.TrimSpace(out)
	if err != nil && out == "" {
		return "", fmt.Errorf("systemctl %s %s: %w", verb, unit, err)
	}
	return out, nil
}

func (s *systemctl) Enabled(ctx context.Context, unit string) (bool, error) {
	st, err := s.state(ctx, "is-enabled", unit)
	return st == "enabled" || st == "enabled-runtime", err
}

func (s *systemctl) Active(ctx context.Context, unit string) (bool, error) {
	st, err := s.state(ctx, "is-active", unit)
	return st == "active", err
}

func (s *systemctl) Do(ctx context.Context, args ...string) error {
	if out, err := s.c.Run(ctx, "systemctl", args...); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

type osAccounts struct{ c Commands }

func (a *osAccounts) User(name string) (int, int, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	return uid, gid, err1 == nil && err2 == nil
}

func (a *osAccounts) Group(name string) (int, bool) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, false
	}
	gid, err := strconv.Atoi(g.Gid)
	return gid, err == nil
}

func (a *osAccounts) AddSystemUser(ctx context.Context, name string) error {
	_, err := a.c.Run(ctx, "useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", name)
	return err
}

func (a *osAccounts) AddSystemGroup(ctx context.Context, name string) error {
	_, err := a.c.Run(ctx, "groupadd", "-f", "--system", name)
	return err
}

type osFiles struct{}

func (osFiles) Owner(path string) (int, int, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: no owner", path)
	}
	return int(st.Uid), int(st.Gid), nil
}

func (osFiles) Chown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

func (osFiles) Rename(from, to string) error { return os.Rename(from, to) }

type httpsFetcher struct{ client *http.Client }

func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("redirect away from https refused")
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

func (f httpsFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("%s: only https downloads", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func freeMB(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize) / (1 << 20)), nil
}

// listening reports whether a TCP socket listens on port, from the kernel's
// tables (/proc/net/tcp and tcp6 under root).
func listening(root string, port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, name := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(strings.TrimSuffix(root, "/") + name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			// sl local_address rem_address st …; 0A is LISTEN.
			if len(fl) > 3 && strings.HasSuffix(fl[1], want) && fl[3] == "0A" {
				f.Close()
				return true
			}
		}
		f.Close()
	}
	return false
}
