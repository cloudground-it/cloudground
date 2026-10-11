package release

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"golang.org/x/sys/unix"
)

// Precompression (SPEC §4): nginx's gzip_static serves a ready-made .gz
// instead of compressing the asset on every request. Only text formats are
// worth it; images, fonts and video are compressed already.
var compressible = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".svg": true, ".json": true,
	".xml": true, ".html": true, ".txt": true, ".map": true,
}

// Assets outside these sizes are not precompressed: below, the gzip framing
// eats the gain; above, the deploy would spend its time on one file.
const (
	minGzip = 1 << 10
	maxGzip = 10 << 20
)

// Precompress writes a .gz next to every compressible file under rel that
// lacks a current one, in parallel, and returns how many it wrote. It is
// for a tree provisioning just made; a deploy does the same as it hashes
// the release.
func Precompress(ctx context.Context, r *sitefs.Root, rel string, workers int) (int, error) {
	res, err := scan(ctx, r, rel, workers, false)
	return res.compressed, err
}

// file is one regular file or symlink of the tree being scanned.
type file struct {
	rel     string
	size    int64
	mtime   time.Time
	link    string // a symlink's target
	isLink  bool
	gzip    bool // write rel.gz
	stale   bool // a .gz exists that gzip replaces (or removes)
	skip    bool // a stale .gz: its digest comes from the rebuild
	sum     [sha256.Size]byte
	gzSum   [sha256.Size]byte
	gzWrote bool
}

type scanResult struct {
	checksum   string
	compressed int
}

var errNotSmaller = errors.New("gzip is not smaller")

// scan walks rel once, then compresses (and, for a release, hashes) its
// files on workers goroutines. A release also loses group and other write
// permission on the way: nothing in it is writable by nginx's group or by
// other users. The checksum covers every regular file, the .gz written here
// included, and every symlink, by path (SPEC §5).
func scan(ctx context.Context, r *sitefs.Root, rel string, workers int, withSum bool) (scanResult, error) {
	var files []*file
	byRel := map[string]*file{}
	taken := map[string]bool{} // every name in the tree
	err := r.Walk(rel, func(p string, fi fs.FileInfo) error {
		taken[p] = true
		m := fi.Mode()
		if withSum && (m.IsDir() || m.IsRegular()) && m.Perm()&0o022 != 0 {
			if err := r.Chmod(p, m.Perm()&^0o022); err != nil {
				return err
			}
		}
		switch {
		case m.IsRegular():
			f := &file{rel: p, size: fi.Size(), mtime: fi.ModTime()}
			files = append(files, f)
			byRel[p] = f
		case m&fs.ModeSymlink != 0 && withSum:
			t, err := r.Readlink(p)
			if err != nil {
				return err
			}
			files = append(files, &file{rel: p, isLink: true, link: t})
		}
		return nil
	})
	if err != nil {
		return scanResult{}, err
	}
	var jobs []*file
	for _, f := range files {
		if f.isLink {
			continue
		}
		if compressible[strings.ToLower(path.Ext(f.rel))] && f.size >= minGzip && f.size <= maxGzip {
			// gzip_static serves an existing .gz without comparing it with
			// the source, so one older than its source (carried over from a
			// staging tree, or left by an in-place update) is rebuilt, or
			// browsers get the old asset while plain clients get the new.
			gz, ok := byRel[f.rel+".gz"]
			switch {
			case ok && !gz.mtime.Before(f.mtime):
				// current
			case ok:
				f.gzip, f.stale, gz.skip = true, true, true
			default:
				// Something that is not a regular file (a directory, a link)
				// under the .gz name is left alone.
				f.gzip = !taken[f.rel+".gz"]
			}
		}
	}
	for _, f := range files {
		if !f.isLink && !f.skip && (f.gzip || withSum) {
			jobs = append(jobs, f)
		}
	}
	if err := parallel(ctx, jobs, workers, func(w *worker, f *file) error { return w.do(r, f, withSum) }); err != nil {
		return scanResult{}, err
	}

	var res scanResult
	type entry struct {
		rel string
		f   *file
		gz  bool
	}
	var all []entry
	for _, f := range files {
		if f.gzWrote {
			res.compressed++
			all = append(all, entry{f.rel + ".gz", f, true})
		}
		if !f.skip {
			all = append(all, entry{f.rel, f, false})
		}
	}
	if !withSum {
		return res, nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].rel < all[j].rel })
	h := sha256.New()
	for _, e := range all {
		name := strings.TrimPrefix(e.rel, rel+"/")
		// Length-prefixed, so {"ab","c"} and {"a","bc"} cannot collide.
		fmt.Fprintf(h, "%d:%s\x00", len(name), name)
		switch {
		case e.f.isLink:
			fmt.Fprintf(h, "L%d:%s", len(e.f.link), e.f.link)
		case e.gz:
			h.Write([]byte{'F'})
			h.Write(e.f.gzSum[:])
		default:
			h.Write([]byte{'F'})
			h.Write(e.f.sum[:])
		}
	}
	res.checksum = hex.EncodeToString(h.Sum(nil))
	return res, nil
}

// parallel runs fn over jobs on n goroutines, each with its own worker,
// and returns the first error.
func parallel(ctx context.Context, jobs []*file, n int, fn func(*worker, *file) error) error {
	if n < 1 {
		n = 1
	}
	ch := make(chan *file)
	errc := make(chan error, n)
	var wg sync.WaitGroup
	for range min(n, max(len(jobs), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newWorker()
			for f := range ch {
				if err := fn(w, f); err != nil {
					errc <- err
					for range ch { // drain, so the sender finishes
					}
					return
				}
			}
		}()
	}
	var err error
send:
	for _, f := range jobs {
		select {
		case ch <- f:
		case err = <-errc:
			break send
		case <-ctx.Done():
			err = ctx.Err()
			break send
		}
	}
	close(ch)
	wg.Wait()
	if err == nil {
		select {
		case err = <-errc:
		default:
		}
	}
	return err
}

// worker holds what one goroutine reuses from file to file.
type worker struct {
	buf []byte
	zw  *gzip.Writer
	h   hash.Hash
	gh  hash.Hash
}

func newWorker() *worker {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestCompression)
	return &worker{buf: make([]byte, 128<<10), zw: zw, h: sha256.New(), gh: sha256.New()}
}

// do hashes f (withSum) and writes its .gz (f.gzip), reading it once. The
// file is opened without following a link and checked on the descriptor.
func (w *worker) do(r *sitefs.Root, f *file, withSum bool) error {
	in, err := r.OpenFile(f.rel, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w", f.rel, sitefs.ErrNotRegular)
	}
	w.h.Reset()
	var src io.Reader = in
	if withSum {
		src = io.TeeReader(in, w.h)
	}
	if !f.gzip {
		if _, err := io.CopyBuffer(io.Discard, src, w.buf); err != nil {
			return err
		}
		w.h.Sum(f.sum[:0])
		return nil
	}
	read := false
	err = r.ReplaceUnsynced(f.rel+".gz", 0o644, func(out *os.File) error {
		w.gh.Reset()
		n := &counter{w: io.MultiWriter(out, w.gh)}
		w.zw.Reset(n)
		if _, err := io.CopyBuffer(w.zw, src, w.buf); err != nil {
			return err
		}
		read = true
		if err := w.zw.Close(); err != nil {
			return err
		}
		if n.n >= f.size {
			return errNotSmaller
		}
		return nil
	})
	switch {
	case err == nil:
		f.gzWrote = true
		w.gh.Sum(f.gzSum[:0])
	case errors.Is(err, errNotSmaller):
		// A stale .gz of an asset that no longer compresses would go on
		// being served in its place.
		if f.stale {
			if err := r.Remove(f.rel + ".gz"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	default:
		return err
	}
	if withSum {
		if !read {
			return fmt.Errorf("%s: read for hashing failed", f.rel)
		}
		w.h.Sum(f.sum[:0])
	}
	return nil
}

// counter counts what passes through to w.
type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
