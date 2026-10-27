package runtimes

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	rt "github.com/cloudground-it/cloudground/internal/runtimes"
)

// Download bounds (SPEC §2).
const (
	maxIndex   = 8 << 20
	maxSums    = 64 << 10
	maxTarball = 128 << 20
)

// NodeInstaller fetches, checks and unpacks one Node.js major (SPEC §2).
type NodeInstaller struct {
	Base   string // "https://nodejs.org/dist"
	Dir    string // the runtimes' directory
	Arch   string // "x64", "arm64"
	Client *http.Client
}

// nodeArch is this machine's architecture as Node.js names it.
func nodeArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x64"
}

// InstallMain is `panel-agent runtime-install node <major>`, run as root in
// its transient unit: it prints {"version": …} or the error (exit 1).
func InstallMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != rt.Node || !rt.ValidVersion(rt.Node, args[1]) {
		fmt.Fprintln(stderr, "usage: runtime-install node <20|22|24>")
		return 2
	}
	n := NodeInstaller{Base: "https://nodejs.org/dist", Dir: rt.Dir, Arch: nodeArch()}
	v, err := n.Install(context.Background(), args[1])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	b, _ := json.Marshal(map[string]string{"version": v})
	fmt.Fprintln(stdout, string(b))
	return 0
}

func (n NodeInstaller) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	base, _ := url.Parse(n.Base)
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 5 || r.URL.Scheme != "https" || base == nil || r.URL.Host != base.Host {
			return errors.New("redirected away from nodejs.org")
		}
		return nil
	}}
}

// get reads a URL, at most max bytes.
func (n NodeInstaller) get(ctx context.Context, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := n.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", u, max)
	}
	return b, nil
}

// Install puts the newest release of major in <Dir>/node/<major>.
func (n NodeInstaller) Install(ctx context.Context, major string) (string, error) {
	idx, err := n.get(ctx, n.Base+"/index.json", maxIndex)
	if err != nil {
		return "", err
	}
	version, err := rt.PickNode(idx, major)
	if err != nil {
		return "", err
	}
	sums, err := n.get(ctx, n.Base+"/"+version+"/SHASUMS256.txt", maxSums)
	if err != nil {
		return "", err
	}
	name := rt.TarballName(version, n.Arch)
	want, err := rt.ParseShasums(string(sums), name)
	if err != nil {
		return "", err
	}
	tarball, err := n.get(ctx, n.Base+"/"+version+"/"+name, maxTarball)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(tarball)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("%s: SHA-256 %s, but SHASUMS256.txt says %s: refused", name, got, want)
	}
	parent := filepath.Join(n.Dir, rt.Node)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(parent, ".new-"+major+"-"+hex.EncodeToString(rnd[:]))
	defer os.RemoveAll(tmp)
	if err := Unpack(strings.NewReader(string(tarball)), tmp); err != nil {
		return "", fmt.Errorf("unpack %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		return "", err
	}
	dst := filepath.Join(parent, major)
	old := filepath.Join(parent, ".old-"+major+"-"+hex.EncodeToString(rnd[:]))
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return "", err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return version, nil
}

// Unpack extracts a .tar.gz into dir (made 0755), without its top
// directory (SPEC §2 step 4): absolute names, .. components, links that
// leave the tree and anything but files, directories and symbolic links
// are refused; set-id bits are dropped.
func Unpack(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := h.Name
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("%q: an absolute name", name)
		}
		parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
		for _, p := range parts {
			if p == ".." {
				return fmt.Errorf("%q: a .. component", name)
			}
		}
		if len(parts) < 2 {
			continue // the top directory itself
		}
		rel := path.Join(parts[1:]...)
		if rel == "." || rel == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		mode := os.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := h.Linkname
			resolved := path.Clean(path.Join(path.Dir(rel), link))
			if strings.HasPrefix(link, "/") || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("%q: a link out of the tree (%s)", name, link)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%q: not a file, a directory or a link", name)
		}
	}
}
