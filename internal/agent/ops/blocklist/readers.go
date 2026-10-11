package blocklist

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	osuser "os/user"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	bl "github.com/cloudground-it/cloudground/internal/blocklist"
)

// sshUnits are the units sshd runs in on Ubuntu (ssh.service) and on
// distributions that name it sshd.service.
var sshUnits = []string{"ssh.service", "sshd.service"}

// followJournal follows sshd's journal until ctx ends or journalctl stops
// (SPEC §1.1).
func (o *Ops) followJournal(ctx context.Context, started func()) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		args := []string{"-f", "-n", "0", "-q", "-o", "json"}
		for _, u := range sshUnits {
			args = append(args, "-u", u)
		}
		// The deadline is the agent's lifetime: a year, then a fresh start.
		_, err := o.Exec.Run(ctx, host.Cmd{Name: "journalctl", Args: args, Stdout: pw, Timeout: 365 * 24 * time.Hour})
		pw.CloseWithError(err)
		done <- err
	}()
	started()
	o.readJournal(pr)
	pr.Close()
	err := <-done
	if err == nil {
		err = errors.New("journalctl ended")
	}
	return err
}

// readJournal counts the sshd failures of a journal stream (-o json, one
// entry per line).
func (o *Ops) readJournal(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var raw map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &raw) != nil {
			continue
		}
		// Trusted fields only (SPEC §1.1): root, in the SSH unit.
		if field(raw["_UID"]) != "0" || !isSSHUnit(field(raw["_SYSTEMD_UNIT"])) {
			continue
		}
		if a, ok := bl.SSHFailure(field(raw["MESSAGE"])); ok {
			o.record(a, bl.SourceSSH, 1)
		}
	}
}

func isSSHUnit(u string) bool {
	for _, s := range sshUnits {
		if u == s {
			return true
		}
	}
	return false
}

// field decodes a journal field: a string, or an array of bytes for a value
// that is not valid UTF-8; anything else reads as "".
func field(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var ints []int
	if json.Unmarshal(v, &ints) == nil {
		b := make([]byte, 0, len(ints))
		for _, c := range ints {
			b = append(b, byte(c))
		}
		return string(b)
	}
	return ""
}

// listen reads the vhosts' failure log socket until ctx ends (SPEC §1.2).
func (o *Ops) listen(ctx context.Context, started func()) error {
	uid, gid, err := o.nginxIDs()
	if err != nil {
		return fmt.Errorf("nginx's user: %w", err)
	}
	path := o.Socket
	if path == "" {
		path = DefaultSocket
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	defer os.Remove(path)
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, 0o620); err != nil {
		return err
	}
	if err := passCredentials(c); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	started()
	buf := make([]byte, 2048)
	oob := make([]byte, credSpace)
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !sentBy(oob[:oobn], uid) {
			continue
		}
		if a, count, ok := bl.NginxFailure(buf[:n]); ok {
			o.record(a, bl.SourceWordPress, count)
		}
	}
}

// nginxIDs resolves the user nginx's workers run as.
func (o *Ops) nginxIDs() (int, int, error) {
	name := o.NginxUser
	if name == "" {
		name = "www-data"
	}
	if o.LookupUser != nil {
		return o.LookupUser(name)
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(strings.TrimSpace(u.Gid))
	return uid, gid, err
}
