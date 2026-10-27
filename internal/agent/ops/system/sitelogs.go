package system

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Downloads of a site's access and error logs (#84, SPEC §3b).

// siteLogDir is the site's log directory, a real directory under LogRoot.
func (o *Ops) siteLogDir(s state.Site) (string, error) {
	if err := site.ValidateDomain(s.Domain); err != nil {
		return "", agentrpc.Invalid("invalid site %q", s.Domain)
	}
	dir := filepath.Join(o.Paths.LogRoot, s.Domain)
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", agentrpc.Invalid("%s is not a directory", dir)
	}
	return dir, nil
}

var rotatedRe = regexp.MustCompile(`^(?:access|error)\.log\.([0-9]+)`)

// ListSiteLogs lists the site's access and error logs, current and rotated.
// Only regular files named as nginx and logrotate name them are listed.
func (o *Ops) ListSiteLogs(ctx context.Context, p rpc.SiteLogsParams) (rpc.SiteLogsResult, error) {
	res := rpc.SiteLogsResult{Files: []rpc.SiteLogFile{}}
	dir, err := o.siteLogDir(p.Site)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		if !rpc.ValidSiteLogName(e.Name()) {
			continue
		}
		fi, err := e.Info() // lstat: a link is not a regular file
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		res.Files = append(res.Files, rpc.SiteLogFile{Name: e.Name(), Size: fi.Size(), Modified: fi.ModTime().UTC()})
	}
	// The current logs first (access, error), then each kind's rotated files
	// from the newest: access.log.1, access.log.2.gz, …
	key := func(name string) (rotated int, kind string, n int) {
		kind = "access"
		if name[0] == 'e' {
			kind = "error"
		}
		if m := rotatedRe.FindStringSubmatch(name); m != nil {
			n, _ = strconv.Atoi(m[1])
			return 1, kind, n
		}
		return 0, kind, 0
	}
	sort.Slice(res.Files, func(i, j int) bool {
		ri, ki, ni := key(res.Files[i].Name)
		rj, kj, nj := key(res.Files[j].Name)
		switch {
		case ri != rj:
			return ri < rj
		case ki != kj:
			return ki < kj
		}
		return ni < nj
	})
	return res, nil
}

// ReadSiteLogChunk reads part of one log. The file is opened inside the
// site's log directory only (os.Root), never through a link, and must be a
// regular file; a FIFO cannot hang the agent.
func (o *Ops) ReadSiteLogChunk(ctx context.Context, p rpc.SiteLogChunkParams) (rpc.SiteLogChunkResult, error) {
	if !rpc.ValidSiteLogName(p.Name) {
		return rpc.SiteLogChunkResult{}, agentrpc.Invalid("invalid log file %q", p.Name)
	}
	dir, err := o.siteLogDir(p.Site)
	if errors.Is(err, os.ErrNotExist) {
		return rpc.SiteLogChunkResult{}, agentrpc.NotFound("no such log")
	}
	if err != nil {
		return rpc.SiteLogChunkResult{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return rpc.SiteLogChunkResult{}, err
	}
	defer root.Close()
	if fi, err := root.Lstat(p.Name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rpc.SiteLogChunkResult{}, agentrpc.NotFound("no such log")
		}
		return rpc.SiteLogChunkResult{}, err
	} else if !fi.Mode().IsRegular() {
		return rpc.SiteLogChunkResult{}, agentrpc.Invalid("%s is not a regular file", p.Name)
	}
	f, err := root.OpenFile(p.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rpc.SiteLogChunkResult{}, agentrpc.NotFound("no such log")
		}
		return rpc.SiteLogChunkResult{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return rpc.SiteLogChunkResult{}, err
	}
	if !fi.Mode().IsRegular() {
		return rpc.SiteLogChunkResult{}, agentrpc.Invalid("%s is not a regular file", p.Name)
	}
	size := fi.Size()
	if p.Offset >= size {
		return rpc.SiteLogChunkResult{Data: []byte{}, Size: size}, nil
	}
	buf := make([]byte, min(int64(p.Length), size-p.Offset))
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return rpc.SiteLogChunkResult{}, err
	}
	return rpc.SiteLogChunkResult{Data: buf[:n], Size: size}, nil
}
