package host

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// An SFTP backup destination with the panel's own key (#23; SPEC: restic,
// SFTP key). The key is written to a root-only file and named in restic's
// sftp.command, never an argument of its own or a variable.

// DefaultSSHDir holds the backup key and the destinations' host keys.
const DefaultSSHDir = "/var/lib/cloudground-agent/backup-ssh"

// SFTPTarget reads the user, host and port of an sftp: repository URL:
// sftp:[user@]host:/path or sftp://[user@]host[:port]//path. A user or
// host starting with "-" is refused: ssh would read it as an option.
func SFTPTarget(repo string) (user, hostname string, port int, err error) {
	bad := fmt.Errorf("not an sftp repository: %w", ErrInvalidArgument)
	port = 22
	switch {
	case strings.HasPrefix(repo, "sftp://"):
		u, perr := url.Parse(repo)
		if perr != nil || u.Host == "" {
			return "", "", 0, bad
		}
		if u.User != nil {
			user = u.User.Username()
		}
		hostname = u.Hostname()
		if p := u.Port(); p != "" {
			n, cerr := strconv.Atoi(p)
			if cerr != nil || n < 1 || n > 65535 {
				return "", "", 0, bad
			}
			port = n
		}
	case strings.HasPrefix(repo, "sftp:"):
		rest := strings.TrimPrefix(repo, "sftp:")
		at, _, ok := strings.Cut(rest, ":")
		if !ok || at == "" {
			return "", "", 0, bad
		}
		if u, h, has := strings.Cut(at, "@"); has {
			user, hostname = u, h
		} else {
			hostname = at
		}
		hostname = strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]")
	default:
		return "", "", 0, bad
	}
	if hostname == "" || strings.HasPrefix(hostname, "-") || strings.HasPrefix(user, "-") ||
		strings.ContainsAny(user+hostname, " \t\n\"'\\") {
		return "", "", 0, bad
	}
	return user, hostname, port, nil
}

// sshDir is where the key and the host keys live.
func (r *ResticCLI) sshDir() string {
	if r.SSHDir != "" {
		return r.SSHDir
	}
	return DefaultSSHDir
}

// sftpArgs writes repo's SSH key and returns restic's -o sftp.command
// arguments for it; none for a repository without a key.
func (r *ResticCLI) sftpArgs(repo Repo) ([]string, error) {
	if repo.SSHKey == "" {
		return nil, nil
	}
	user, hostname, port, err := SFTPTarget(repo.URL)
	if err != nil {
		return nil, err
	}
	dir := r.sshDir()
	if strings.ContainsAny(dir, " \t\n\"'\\") || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("restic: ssh folder %q: %w", dir, ErrInvalidArgument)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	key := filepath.Join(dir, "id_ed25519")
	tmp, err := os.CreateTemp(dir, ".key-")
	if err != nil {
		return nil, err
	}
	data := repo.SSHKey
	if !strings.HasSuffix(data, "\n") {
		data += "\n"
	}
	_, werr := tmp.WriteString(data)
	cerr := tmp.Close()
	if werr = errors.Join(werr, cerr, os.Chmod(tmp.Name(), 0o600)); werr != nil {
		os.Remove(tmp.Name())
		return nil, werr
	}
	if err := os.Rename(tmp.Name(), key); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	target := hostname
	if user != "" {
		target = user + "@" + hostname
	}
	cmd := strings.Join([]string{
		"ssh", target, "-p", strconv.Itoa(port), "-i", key,
		"-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"), "-s", "sftp",
	}, " ")
	return []string{"-o", "sftp.command=" + cmd}, nil
}
