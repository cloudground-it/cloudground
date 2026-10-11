package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
)

// SSHPorts lists the ports sshd listens on, in the order its configuration
// gives them (SPEC.md §5). It may be empty when nothing can be read.
func SSHPorts(ctx context.Context, e host.Exec, sshdConfig string) []int {
	res, err := e.Run(ctx, host.Cmd{Name: "sshd", Args: []string{"-T"}, Timeout: 15 * time.Second})
	if err == nil {
		if ports := sshdPorts(string(res.Stdout)); len(ports) > 0 {
			return ports
		}
	}
	// The effective configuration is unavailable (a broken config, a
	// missing host key): read the files themselves.
	files := []string{sshdConfig}
	if extra, _ := filepath.Glob(filepath.Join(filepath.Dir(sshdConfig), "sshd_config.d", "*.conf")); len(extra) > 0 {
		files = append(files, extra...)
	}
	var text strings.Builder
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			text.Write(b)
			text.WriteByte('\n')
		}
	}
	return sshdPorts(text.String())
}

// SSHPort is the port to give users for SFTP: sshd's first one, else 22.
func SSHPort(ctx context.Context, e host.Exec, sshdConfig string) int {
	if p := SSHPorts(ctx, e, sshdConfig); len(p) > 0 {
		return p[0]
	}
	return 22
}

// sshdPorts reads `listenaddress host:N` and `port N` lines, the keyword in
// any case, as both `sshd -T` and sshd_config write them. The ports of
// ListenAddress come first: `sshd -T` prints one per socket sshd actually
// opens, and a ListenAddress with its own port makes sshd listen there even
// when Port says otherwise. The Port lines follow, so the essential set is
// never smaller than either.
func sshdPorts(text string) []int {
	var listen, declared []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "port":
			declared = append(declared, f[1])
		case "listenaddress":
			// host:port or [v6]:port; a bare address has no port of its own.
			if _, port, err := net.SplitHostPort(f[1]); err == nil {
				listen = append(listen, port)
			}
		}
	}
	var out []int
	seen := map[int]bool{}
	for _, s := range append(listen, declared...) {
		n, err := strconv.Atoi(s)
		if err == nil && n > 0 && n < 65536 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// SSHDReloader is what ApplySSHDDropIn needs of systemd.
type SSHDReloader interface {
	Reload(ctx context.Context, unit string) error
	UnitProps(ctx context.Context, unit string) (host.UnitProps, error)
}

// sshdUnits are the names sshd's service has: ssh.service on Debian and
// Ubuntu, sshd.service elsewhere.
var sshdUnits = []string{"ssh.service", "sshd.service"}

// ApplySSHDDropIn makes the sshd_config drop-in at path hold content
// (SPEC.md §5.1). Nothing is done when it already does. Otherwise the file
// is replaced (root, 0644), `sshd -t` checks the whole configuration and,
// when it refuses it, the previous file is put back (or the new one
// removed) and the error returned: sshd is never reloaded on a
// configuration it refuses. Then sshd is reloaded; a unit that is not
// running reads the file when it starts. changed reports a new file.
func ApplySSHDDropIn(ctx context.Context, e host.Exec, sd SSHDReloader, path string, content []byte) (changed bool, err error) {
	old, rerr := rootfile.ReadNoFollow(path)
	existed := rerr == nil
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, rerr)
	}
	if existed && bytes.Equal(old, content) {
		return false, nil
	}
	if err := rootfile.Replace(path, content, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if _, terr := e.Run(ctx, host.Cmd{Name: "sshd", Args: []string{"-t"}, Timeout: 15 * time.Second}); terr != nil {
		var undo error
		if existed {
			undo = rootfile.Replace(path, old, 0o644)
		} else {
			undo = os.Remove(path)
		}
		if undo != nil {
			return false, fmt.Errorf("sshd refused %s (%v), and putting the previous file back failed: %w", path, terr, undo)
		}
		return false, fmt.Errorf("sshd refused %s; the previous configuration is back: %w", path, terr)
	}
	return true, reloadSSHD(ctx, sd)
}

// reloadSSHD reloads the first of sshdUnits that runs. None running (socket
// activation before the first connection) is not an error.
func reloadSSHD(ctx context.Context, sd SSHDReloader) error {
	var errs []error
	for _, u := range sshdUnits {
		p, err := sd.UnitProps(ctx, u)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p.ActiveState != "active" && p.ActiveState != "reloading" {
			continue
		}
		if err := sd.Reload(ctx, u); err != nil {
			return fmt.Errorf("reload sshd: %w", err)
		}
		return nil
	}
	if len(errs) == len(sshdUnits) {
		return fmt.Errorf("reload sshd: %w", errors.Join(errs...))
	}
	return nil
}
