package panel

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Size limits for what a release may serve.
const (
	maxManifest  = 1 << 20
	maxSignature = 4 << 10
	maxArtifact  = 256 << 20
)

func (o *Ops) updateClient() *http.Client {
	if o.UpdateClient != nil {
		return o.UpdateClient
	}
	return &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Scheme)
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// get answers url's body, refusing anything but 200.
func get(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// fetchSmall reads a whole small file, at most limit bytes.
func fetchSmall(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	body, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: more than %d bytes", url, limit)
	}
	return b, nil
}

// fetchTo streams url into a new file at path (which must not exist: it is
// created O_EXCL and never through a symlink) with exactly mode, and returns
// its sha256 in hex.
func fetchTo(ctx context.Context, client *http.Client, url, path string, mode os.FileMode) (string, error) {
	body, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxArtifact+1))
	if err == nil && n > maxArtifact {
		err = fmt.Errorf("%s: more than %d bytes", url, maxArtifact)
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// nativeExecutable refuses anything that is not an ELF executable for this
// machine: a truncated download, an HTML error page, another architecture.
func nativeExecutable(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("not an executable: %w", err)
	}
	defer f.Close()
	want, ok := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if !ok || f.Machine != want {
		return fmt.Errorf("built for %v, this machine is %s", f.Machine, runtime.GOARCH)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("not an executable (ELF type %v)", f.Type)
	}
	return nil
}

// stagingName is where an artifact is staged, next to its live name so the
// swap is a rename on one filesystem.
func stagingName(live string) string {
	return filepath.Join(filepath.Dir(live), "."+filepath.Base(live)+".cg-update")
}

func parentDir(p string) string {
	return filepath.Dir(strings.TrimRight(p, "/"))
}

const nofollow = unix.O_NOFOLLOW

// ownerUID is a file's owner, or -1 when the platform does not say.
func ownerUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// owner is a file's uid and gid, or -1, -1 when the platform does not say.
func owner(fi os.FileInfo) (int, int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// sameFile reports whether two paths name the same inode, following
// symlinks (a /proc/<pid>/exe link included).
func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}
