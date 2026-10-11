package version

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// VerifyRelease checks a release directory as it was downloaded: its
// SHA256SUMS must carry a valid SHA256SUMS.sig (VerifyManifest), and each
// file in names must be listed there and match its signed checksum. With no
// names, every listed file must be present and match. A file is read only
// if it is a regular file (a symlink is refused, not followed). It is what
// `cgctl release verify` runs, and the directory form the installer
// should call (SPEC §3.3).
func VerifyRelease(dir string, names ...string) (Manifest, error) {
	body, err := readSmall(filepath.Join(dir, ManifestName), 1<<20)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := readSmall(filepath.Join(dir, SignatureName), 4<<10)
	if err != nil {
		return Manifest{}, err
	}
	m, err := VerifyManifest(body, sig)
	if err != nil {
		return Manifest{}, err
	}
	if len(names) == 0 {
		names = slices.Sorted(maps.Keys(m.Sums))
	}
	for _, name := range names {
		want, ok := m.Sums[name]
		if !ok {
			return Manifest{}, fmt.Errorf("%s is not in the signed manifest", name)
		}
		got, err := fileSum(filepath.Join(dir, name))
		if err != nil {
			return Manifest{}, err
		}
		if got != want {
			return Manifest{}, fmt.Errorf("%s does not match the signed checksum: refusing to install", name)
		}
	}
	return m, nil
}

func regular(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := regular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return b, nil
}

func fileSum(path string) (string, error) {
	f, err := regular(path)
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
