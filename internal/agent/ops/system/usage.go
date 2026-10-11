package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

// siteSliceDir is where systemd puts a site's slice in the cgroup tree:
// cg-site-<id>.slice is a child of cg-site.slice, itself of cg.slice.
func (o *Ops) siteSliceDir(systemUser string) string {
	root := o.Paths.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}
	return filepath.Join(root, "cg.slice", "cg-site.slice", site.Slice(systemUser))
}

// SiteUsage reads each site's slice counters, and with t.Disk walks its
// tree and logs when the target asks (api SPEC §11.5). Nothing is written: the reads are of
// cgroup files and of the sizes in directory entries, never of a file's
// content, and the walk follows no link.
func (o *Ops) SiteUsage(ctx context.Context, p rpc.SiteUsageParams) (rpc.SiteUsageResult, error) {
	out := rpc.SiteUsageResult{Sites: make([]rpc.SiteUsage, 0, len(p.Sites))}
	for _, t := range p.Sites {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		u := o.sliceUsage(t)
		u.DiskBytes, u.DiskInodes = -1, -1
		if t.Disk {
			var bytes, inodes int64
			ok := true
			for _, dir := range []string{filepath.Join(o.Paths.SitesRoot, t.Domain), filepath.Join(o.Paths.LogRoot, t.Domain)} {
				b, n, err := diskUse(ctx, dir)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					ok = false
					o.log().Warn("reading a site's disk use", "site", t.Domain, "err", err)
				}
				bytes, inodes = bytes+b, inodes+n
			}
			if ok {
				u.DiskBytes, u.DiskInodes = bytes, inodes
			}
		}
		out.Sites = append(out.Sites, u)
	}
	return out, nil
}

// sliceUsage reads one slice; a slice that is not there (no process of the
// site is running) reads as not running.
func (o *Ops) sliceUsage(t rpc.UsageTarget) rpc.SiteUsage {
	u := rpc.SiteUsage{ID: t.ID}
	dir := o.siteSliceDir(t.SystemUser)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return u
	}
	u.Running = true
	u.CPUUsec = keyed(filepath.Join(dir, "cpu.stat"), "usage_usec")
	u.MemoryBytes = single(filepath.Join(dir, "memory.current"))
	u.MemoryMaxBytes = single(filepath.Join(dir, "memory.max"))
	u.Tasks = single(filepath.Join(dir, "pids.current"))
	u.OOMKills = keyed(filepath.Join(dir, "memory.events"), "oom_kill")
	u.IOReadBytes, u.IOWriteBytes = ioBytes(filepath.Join(dir, "io.stat"))
	return u
}

// single is a cgroup file holding one number; "max", or a missing file, is 0.
func single(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// keyed is the value of key in a "key value" cgroup file.
func keyed(path, key string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// ioBytes sums rbytes and wbytes over the devices of an io.stat.
func ioBytes(path string) (read, write int64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, f := range strings.Fields(line) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(v, 10, 64)
			switch k {
			case "rbytes":
				read += n
			case "wbytes":
				write += n
			}
		}
	}
	return read, write
}

// diskUse is the space dir's entries take on disk (allocated blocks, so a
// sparse file counts what it uses) and how many there are. It does not
// follow links and does not cross into another file system.
func diskUse(ctx context.Context, dir string) (bytes, inodes int64, err error) {
	root, err := os.Lstat(dir)
	if err != nil {
		return 0, 0, err
	}
	dev := uint64(0)
	if st, ok := root.Sys().(*syscall.Stat_t); ok {
		dev = uint64(st.Dev)
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that went away during the walk: skip it.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if d.IsDir() && uint64(st.Dev) != dev {
			return filepath.SkipDir
		}
		inodes++
		bytes += st.Blocks * 512
		return nil
	})
	return bytes, inodes, err
}
