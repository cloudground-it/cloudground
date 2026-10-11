package staging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/release"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"golang.org/x/sys/unix"
)

// The diff's bounds (SPEC §3): enough for any theme or plugin change, and a
// pathological tree cannot keep root hashing for minutes.
const (
	maxWalked = 50000
	maxListed = 1000
	maxHashed = 8 << 20
)

// tree is one side of the diff: a site's root and its live release.
type tree struct {
	r     *sitefs.Root
	rel   string
	files map[string]fs.FileInfo // by path inside the release
	full  bool                   // stopped at maxWalked
}

// Diff lists how staging's live release differs from production's
// (SPEC §3).
func (o *Ops) Diff(_ context.Context, p rpc.StagingParams) (rpc.StagingDiffResult, error) {
	var res rpc.StagingDiffResult
	if err := pair(p.Production, p.Staging); err != nil {
		return res, err
	}
	pr, err := o.Open(p.Production)
	if err != nil {
		return res, err
	}
	defer pr.Close()
	sr, err := o.Open(p.Staging)
	if err != nil {
		return res, err
	}
	defer sr.Close()
	prod, err := walkLive(pr)
	if err != nil {
		return res, err
	}
	stg, err := walkLive(sr)
	if err != nil {
		return res, err
	}

	res.Scanned = len(prod.files) + len(stg.files)
	res.Truncated = prod.full || stg.full
	list := func(to *[]string, name string) {
		if len(*to) == maxListed {
			res.Truncated = true
			return
		}
		*to = append(*to, name)
	}
	// Walk in path order, so a truncated list is the first paths, not a
	// random sample of them.
	for _, name := range sortedNames(stg.files) {
		si := stg.files[name]
		pi, ok := prod.files[name]
		switch {
		case !ok:
			list(&res.Added, name)
		case si.Size() != pi.Size():
			list(&res.Modified, name)
		case si.ModTime().Equal(pi.ModTime()):
		case si.Size() > maxHashed || !sameContent(stg, prod, name):
			list(&res.Modified, name)
		}
	}
	for _, name := range sortedNames(prod.files) {
		if _, ok := stg.files[name]; !ok {
			list(&res.Removed, name)
		}
	}
	return res, nil
}

// walkLive lists the regular files of r's live release, skipping uploads
// and .git directories, nothing followed.
func walkLive(r *sitefs.Root) (*tree, error) {
	rel, err := release.Live(r)
	if err != nil {
		return nil, err
	}
	t := &tree{r: r, rel: rel, files: map[string]fs.FileInfo{}}
	err = r.Walk(rel, func(p string, fi fs.FileInfo) error {
		name := strings.TrimPrefix(p, rel+"/")
		if fi.IsDir() && (name == uploadsDir || fi.Name() == ".git") {
			return fs.SkipDir
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if len(t.files) == maxWalked {
			t.full = true
			return fs.SkipAll
		}
		t.files[name] = fi
		return nil
	})
	return t, err
}

func sortedNames(m map[string]fs.FileInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameContent compares the file name of both trees by SHA-256. A file that
// cannot be read (gone, or swapped for a link) is not the same.
func sameContent(a, b *tree, name string) bool {
	da, ok := digest(a, name)
	if !ok {
		return false
	}
	db, ok := digest(b, name)
	return ok && bytes.Equal(da, db)
}

func digest(t *tree, name string) ([]byte, bool) {
	f, err := t.r.OpenFile(t.rel+"/"+name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxHashed+1)); err != nil {
		return nil, false
	}
	return h.Sum(nil), true
}
