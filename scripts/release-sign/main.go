// Command release-sign holds the private side of CloudGround's release
// signatures (internal/version/SPEC.md §3). It runs where a release is
// signed (the release workflow's approved job, or the owner's machine for a
// key ceremony), never on a server.
//
//	release-sign keygen <key-file>        new key: private half to the file (0600), public half on stdout
//	release-sign pubkey <key-file>        the public half of an existing key
//	release-sign sign <key-file> <tag> <dir>
//	release-sign channel <tag>            "stable" or "beta"; fails for anything that is not a release tag
//
// sign writes <dir>/SHA256SUMS ("version <tag>", then "<sha256>  <name>" for
// every regular file in <dir>, sorted by name) and <dir>/SHA256SUMS.sig, the
// base64 ed25519 signature of that file's exact bytes. It refuses a tag that
// is not vX.Y.Z or vX.Y.Z-beta.N, a subdirectory or link in <dir>, and an
// empty <dir>. Checking a signature is not this tool's job: that is
// `cgctl release verify`, the code every server runs.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage: release-sign keygen <key-file> | pubkey <key-file> | sign <key-file> <tag> <dir> | channel <tag>")

func run(args []string, out io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "keygen":
		return keygen(args[1], out)
	case len(args) == 2 && args[0] == "pubkey":
		key, err := readKey(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		return err
	case len(args) == 4 && args[0] == "sign":
		return sign(args[1], args[2], args[3])
	case len(args) == 2 && args[0] == "channel":
		ch, ok := version.ReleaseTag(args[1])
		if !ok {
			return fmt.Errorf("%q is not a release tag (vX.Y.Z or vX.Y.Z-beta.N)", args[1])
		}
		_, err := fmt.Fprintln(out, ch)
		return err
	}
	return errUsage
}

func keygen(path string, out io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return werr
	}
	_, err = fmt.Fprintln(out, base64.StdEncoding.EncodeToString(pub))
	return err
}

func readKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s is not a release-sign private key", path)
	}
	return ed25519.PrivateKey(key), nil
}

func sign(keyPath, tag, dir string) error {
	if _, ok := version.ReleaseTag(tag); !ok {
		return fmt.Errorf("%q is not a release tag (vX.Y.Z or vX.Y.Z-beta.N)", tag)
	}
	key, err := readKey(keyPath)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if name == version.ManifestName || name == version.SignatureName {
			continue
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file: a release is a flat directory of files", filepath.Join(dir, name))
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Errorf("%s holds nothing to sign", dir)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "version %s\n", tag)
	for _, name := range names {
		sum, err := sha256File(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	body := []byte(b.String())
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, body)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, version.ManifestName), body, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, version.SignatureName), []byte(sig), 0o644)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
