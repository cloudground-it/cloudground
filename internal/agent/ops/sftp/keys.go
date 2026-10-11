package sftp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// Where the keys live, relative to the site root (sshd's default
// AuthorizedKeysFile under the user's home, which is the site root).
const (
	sshDir   = ".ssh"
	keysFile = ".ssh/authorized_keys"
	// shellKeysFile holds the keys of type "shell"; sshd reads it for the
	// site's shell login only (SPEC §5.2).
	shellKeysFile = ".ssh/shell_keys"
)

const (
	sshDirMode   = 0o750
	keysFileMode = 0o640
	maxKeysFile  = 1 << 20
	maxLabel     = 120
)

// keyTypes are the public key types accepted (SPEC §4.2).
var keyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// pubKey is one parsed authorized_keys entry.
type pubKey struct {
	typ, data, label string
	sum              [sha256.Size]byte
}

// fingerprint is the form ssh-keygen -l prints.
func (k pubKey) fingerprint() string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(k.sum[:])
}

// legacyFingerprint is the URL-safe form the old agent showed; a delete
// still accepts it.
func (k pubKey) legacyFingerprint() string {
	return "SHA256:" + base64.RawURLEncoding.EncodeToString(k.sum[:])
}

func (k pubKey) line() string {
	if k.label == "" {
		return k.typ + " " + k.data
	}
	return k.typ + " " + k.data + " " + k.label
}

func (k pubKey) rpc() rpc.SSHKey {
	return rpc.SSHKey{Type: k.typ, Fingerprint: k.fingerprint(), Label: k.label}
}

// parseKey reads "<type> <base64> [comment]".
func parseKey(s string) (pubKey, error) {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, "\r\n") {
		return pubKey{}, agentrpc.Invalid("add one SSH public key at a time")
	}
	f := strings.Fields(s)
	if len(f) < 2 || !keyTypes[f[0]] {
		return pubKey{}, agentrpc.Invalid("unsupported or malformed SSH public key")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) > 16<<10 {
		return pubKey{}, agentrpc.Invalid("malformed SSH public key data")
	}
	pk, err := ssh.ParsePublicKey(blob)
	if err != nil || pk.Type() != f[0] {
		return pubKey{}, agentrpc.Invalid("malformed SSH public key data")
	}
	return pubKey{typ: f[0], data: f[1], label: label(f[2:]), sum: sha256.Sum256(blob)}, nil
}

// label makes a key's comment safe to store and show: printable characters,
// single spaces, at most maxLabel bytes cut on a character boundary.
func label(words []string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, strings.Join(words, " "))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLabel {
		s = s[:maxLabel]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// siteLock serialises key changes on one site.
func (o *Ops) siteLock(root string) func() {
	m, _ := o.keyLocks.LoadOrStore(root, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Key types (SPEC §5.2).
const (
	AccessSFTP  = "sftp"
	AccessShell = "shell"
)

// SSHKeys lists, adds or deletes the site user's public keys.
func (o *Ops) SSHKeys(ctx context.Context, p rpc.SSHKeyParams) (rpc.SSHKeysResult, error) {
	var add pubKey
	switch p.Action {
	case "list", "delete":
	case "add":
		switch p.Access {
		case "", AccessSFTP, AccessShell:
		default:
			return rpc.SSHKeysResult{}, agentrpc.Invalid("unsupported SSH key type %q", p.Access)
		}
		k, err := parseKey(p.PublicKey)
		if err != nil {
			return rpc.SSHKeysResult{}, err
		}
		add = k
	default:
		return rpc.SSHKeysResult{}, agentrpc.Invalid("unsupported SSH key action %q", p.Action)
	}
	defer o.siteLock(p.Site.RootPath)()
	r, err := o.Open(p.Site)
	if err != nil {
		return rpc.SSHKeysResult{}, err
	}
	defer r.Close()
	rr := r.WithOwner(sitefs.Owner{UID: 0, GID: r.Owner().GID})
	if err := secureDir(rr); err != nil {
		return rpc.SSHKeysResult{}, err
	}
	lines, err := readLines(rr, keysFile)
	if err != nil {
		return rpc.SSHKeysResult{}, err
	}
	shellLines, err := readLines(rr, shellKeysFile)
	if err != nil {
		return rpc.SSHKeysResult{}, err
	}
	shell := map[[sha256.Size]byte]bool{}
	for _, l := range shellLines {
		if k, err := parseKey(l); err == nil {
			shell[k.sum] = true
		}
	}

	switch p.Action {
	case "add":
		for _, l := range lines {
			if k, err := parseKey(l); err == nil && k.sum == add.sum {
				return rpc.SSHKeysResult{}, agentrpc.Conflict("SSH key already exists")
			}
		}
		lines = append(lines, add.line())
		shell[add.sum] = p.Access == AccessShell
	case "delete":
		kept := lines[:0:0]
		for _, l := range lines {
			if k, err := parseKey(l); err == nil && p.Fingerprint != "" &&
				(k.fingerprint() == p.Fingerprint || k.legacyFingerprint() == p.Fingerprint) {
				delete(shell, k.sum)
				continue
			}
			kept = append(kept, l)
		}
		if len(kept) == len(lines) {
			return rpc.SSHKeysResult{}, agentrpc.NotFound("SSH key not found")
		}
		lines = kept
	}
	var keys []pubKey
	for _, l := range lines {
		if k, err := parseKey(l); err == nil {
			keys = append(keys, k)
		}
	}
	if p.Action != "list" {
		if err := writeLines(rr, keysFile, lines); err != nil {
			return rpc.SSHKeysResult{}, err
		}
		// The shell keys are those of authorized_keys that are marked so;
		// a line of shell_keys for a key no longer there is dropped.
		var sl []string
		for _, k := range keys {
			if shell[k.sum] {
				sl = append(sl, k.line())
			}
		}
		if _, err := rr.Lstat(shellKeysFile); len(sl) > 0 || err == nil {
			if err := writeLines(rr, shellKeysFile, sl); err != nil {
				return rpc.SSHKeysResult{}, err
			}
		}
		if !p.Site.Config.SFTPEnabled {
			// SFTP off: the password stays locked. This also repairs an
			// account the old agent left with an empty password.
			if err := o.Accounts.LockUser(ctx, p.Site.SystemUser); err != nil {
				return rpc.SSHKeysResult{}, fmt.Errorf("lock the SFTP password: %w", err)
			}
		}
	}
	res := rpc.SSHKeysResult{Keys: []rpc.SSHKey{}, Port: 22, ShellUser: site.ShellUser(p.Site.SystemUser)}
	if o.Port != nil {
		res.Port = o.Port(ctx)
	}
	for _, k := range keys {
		key := k.rpc()
		key.Access = AccessSFTP
		if shell[k.sum] {
			key.Access = AccessShell
		}
		res.Keys = append(res.Keys, key)
	}
	return res, nil
}

// secureDir makes .ssh a real folder, root:<site group> 0750, and removes an
// authorized_keys or shell_keys that is not a regular file. r creates for
// root and the site's group.
func secureDir(r *sitefs.Root) error {
	fi, err := r.Lstat(sshDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := r.Mkdir(sshDir, sshDirMode); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return agentrpc.Conflict(".ssh in the site's folder is not a folder; remove it in the file manager")
	default:
		o := r.Owner()
		if err := r.Chown(sshDir, o.UID, o.GID); err != nil {
			return err
		}
		if err := r.Chmod(sshDir, sshDirMode); err != nil {
			return err
		}
	}
	for _, f := range []string{keysFile, shellKeysFile} {
		if fi, err := r.Lstat(f); err == nil && !fi.Mode().IsRegular() {
			if err := r.RemoveAll(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// readLines returns the non-blank lines of a key file (none when it is
// missing).
func readLines(r *sitefs.Root, file string) ([]string, error) {
	b, err := r.ReadFile(file, maxKeysFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out, nil
}

// writeLines replaces a key file with lines, root's and the site group's,
// 0640.
func writeLines(r *sitefs.Root, file string, lines []string) error {
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	return r.Replace(file, keysFileMode, func(f *os.File) error {
		_, err := io.WriteString(f, content)
		return err
	})
}
