package migration

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"golang.org/x/sys/unix"
)

const readNoFollow = unix.O_RDONLY | unix.O_NOFOLLOW

// Limits bound what an archive may make (SPEC §3).
type Limits struct {
	MaxFiles   int   // regular files, hard-link copies included
	MaxEntries int   // entries of any kind, skipped ones included
	MaxBytes   int64 // file content, and the size of a staged input
	MaxRatio   int64 // content per compressed byte
	MinBudget  int64 // content always allowed, whatever the ratio
	Reserve    int64 // free space left on the work directory's filesystem
}

// DefaultLimits are the limits a migration runs with.
var DefaultLimits = Limits{
	MaxFiles:   200_000,
	MaxEntries: 400_000,
	MaxBytes:   20 << 30,
	MaxRatio:   200,
	MinBudget:  64 << 20,
	Reserve:    1 << 30,
}

// streamSlack is what the tar stream may hold beyond the content: headers,
// padding and the data of entries that are skipped.
const streamSlack = 1 << 30

// Budget is how many bytes of content an archive of compressed bytes may
// expand to, given avail bytes free where it is extracted.
func (l Limits) Budget(compressed, avail int64) int64 {
	b := l.MaxBytes
	if l.MaxRatio > 0 {
		r := max(l.MinBudget, satMul(compressed, l.MaxRatio))
		b = min(b, r)
	}
	return max(0, min(b, avail-l.Reserve))
}

// MaxRatioCeiling is the highest expansion limit an administrator may set
// for one migration (MigrationParams.MaxRatio; SPEC §3). A gigabyte of
// zeros is a megabyte of gzip, about 1000 times: above this, a ratio no
// longer tells an archive from a bomb.
const MaxRatioCeiling = rpc.MaxMigrationRatio

// ErrTooLarge marks the error of an archive that expands past its budget.
var ErrTooLarge = errors.New("the archive expands past its budget")

// Keys of the too-large message in the API's catalogue (internal/i18n), by
// the bound that was reached.
const (
	KeyTooLargeRatio = "migration.tooLargeRatio"
	KeyTooLargeSize  = "migration.tooLargeSize"
	KeyTooLargeDisk  = "migration.tooLargeDisk"
)

// TooLarge is the error for an archive of compressed bytes, extracted
// where avail bytes were free, that expanded past l.Budget: it says which
// bound it reached and, for the ratio, how to raise it (SPEC §3). It carries
// a translation key and its values; its message is the English text.
func (l Limits) TooLarge(compressed, avail int64) error {
	budget := humanSize(l.Budget(compressed, avail))
	ratioBudget := int64(1 << 62)
	if l.MaxRatio > 0 {
		ratioBudget = max(l.MinBudget, satMul(compressed, l.MaxRatio))
	}
	e := &agentrpc.Error{Code: agentrpc.CodeInvalid}
	switch {
	case avail-l.Reserve < min(l.MaxBytes, ratioBudget):
		e.Key, e.Args = KeyTooLargeDisk, []string{budget, humanSize(l.Reserve)}
		e.Message = fmt.Sprintf("The archive expands to more than %s, the free disk space for it (%s is kept free). Free some disk space and try again.", e.Args[0], e.Args[1])
	case ratioBudget < l.MaxBytes:
		e.Key, e.Args = KeyTooLargeRatio, []string{budget, strconv.FormatInt(l.MaxRatio, 10), strconv.Itoa(MaxRatioCeiling)}
		e.Message = fmt.Sprintf("The archive expands to more than %s, %s times its compressed size, the limit. An administrator can raise the expansion limit in the migration form, up to %s times.", e.Args[0], e.Args[1], e.Args[2])
	default:
		e.Key, e.Args = KeyTooLargeSize, []string{budget}
		e.Message = fmt.Sprintf("The archive expands to more than %s, the most a migration may unpack.", e.Args[0])
	}
	return e
}

func satMul(a, b int64) int64 {
	if a > 0 && b > 0 && a > (1<<62)/b {
		return 1 << 62
	}
	return a * b
}

// Stats is what an extraction made.
type Stats struct {
	Files   int
	Bytes   int64
	Skipped int
}

type extractor struct {
	r      *sitefs.Root
	lim    Limits
	budget int64
	dirs   map[string]bool // directories known to exist, without a link on them
	seen   map[string]bool // every name a file or directory has taken
	links  []pendingLink
	st     Stats
	n      int
}

type pendingLink struct{ name, target string }

// Unpack extracts the gzip-compressed tar read from in into tree, a Root on
// an empty directory that root alone can reach, writing at most budget
// bytes of content, and returns what it made and the directory of tree that
// holds the site (SPEC §3).
func Unpack(in io.Reader, tree *sitefs.Root, lim Limits, budget int64) (Stats, string, error) {
	st, err := extract(in, tree, lim, budget)
	if err != nil {
		return st, "", err
	}
	src, err := sourceDir(tree)
	if err != nil {
		return st, "", err
	}
	sub, err := tree.Sub(src)
	if err != nil {
		return st, "", err
	}
	defer sub.Close()
	n, err := dropEscaping(sub)
	st.Skipped += n
	return st, src, err
}

// sourceDir is the directory of the tree that holds the site: its one
// top-level entry when that is a real directory, else the tree itself.
func sourceDir(tree *sitefs.Root) (string, error) {
	ents, err := tree.ReadDir(".")
	if err != nil {
		return "", err
	}
	if len(ents) == 1 && ents[0].IsDir() {
		return ents[0].Name(), nil
	}
	return ".", nil
}

// extract unpacks the archive into r. Links are made last and checked
// lexically; Unpack then drops the ones that leave the source directory.
func extract(in io.Reader, r *sitefs.Root, lim Limits, budget int64) (Stats, error) {
	gz, err := gzip.NewReader(in)
	if err != nil {
		return Stats{}, agentrpc.Invalid("the archive is not gzip-compressed: %v", err)
	}
	defer gz.Close()
	stream := &capped{r: gz, left: budget + streamSlack}
	x := &extractor{r: r, lim: lim, budget: budget, dirs: map[string]bool{".": true}, seen: map[string]bool{}}
	tr := tar.NewReader(stream)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if stream.over {
				return x.st, x.tooBig()
			}
			return x.st, agentrpc.Wrap(agentrpc.CodeInvalid, "the archive is not a valid tar.gz", err)
		}
		if err := x.entry(h, tr); err != nil {
			if stream.over {
				return x.st, x.tooBig()
			}
			return x.st, err
		}
	}
	if x.st.Files == 0 {
		return x.st, agentrpc.Invalid("the migration archive holds no files")
	}
	for _, l := range x.links {
		if !x.link(l) {
			x.st.Skipped++
		}
	}
	return x.st, nil
}

// tooBig is the error of an archive past its budget; Import replaces it
// with Limits.TooLarge, which knows the sizes the budget came from.
func (x *extractor) tooBig() error {
	return errors.Join(ErrTooLarge, agentrpc.Invalid("the archive expands to more than %s, its limit (%d times its compressed size, at most %s, within the free disk space)",
		humanSize(x.budget), x.lim.MaxRatio, humanSize(x.lim.MaxBytes)))
}

// humanSize writes n bytes for a person.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// memberName cleans an archive member's name; "" is the tree itself.
func memberName(raw string) (string, error) {
	if strings.IndexByte(raw, 0) >= 0 || path.IsAbs(raw) {
		return "", agentrpc.Invalid("the archive member %q has an absolute path", raw)
	}
	n := path.Clean(raw)
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", agentrpc.Invalid("the archive member %q leads outside the archive", raw)
	}
	if n == "." {
		return "", nil
	}
	return n, nil
}

func (x *extractor) entry(h *tar.Header, data io.Reader) error {
	if h.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	if x.n++; x.n > x.lim.MaxEntries {
		return agentrpc.Invalid("the archive has more than %d entries, its limit", x.lim.MaxEntries)
	}
	name, err := memberName(h.Name)
	if err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if x.seen[name] && !x.dirs[name] {
			return conflict(name)
		}
		x.seen[name] = true
		return x.mkdirAll(name)
	case tar.TypeReg, tar.TypeRegA:
		mode := fs.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		return x.file(name, h.Size, mode, data)
	case tar.TypeLink:
		return x.hardlink(name, h.Linkname)
	case tar.TypeSymlink:
		// Made after everything else, if the name is still free then.
		t := h.Linkname
		if t == "" || path.IsAbs(t) || strings.IndexByte(t, 0) >= 0 {
			x.st.Skipped++
			return nil
		}
		if j := path.Join(path.Dir(name), t); j == ".." || strings.HasPrefix(j, "../") {
			x.st.Skipped++
			return nil
		}
		x.links = append(x.links, pendingLink{name: name, target: t})
		return nil
	default:
		// Devices, FIFOs, sockets and anything unknown have no place in a
		// web site's tree, and a FIFO could hang whatever opens it.
		x.st.Skipped++
		return nil
	}
}

func conflict(name string) error {
	return agentrpc.Invalid("the archive has conflicting entries for %q", name)
}

// mkdirAll makes the directory name and its missing parents. There are no
// links in the tree yet, so none is followed; a file in the way is a
// conflict.
func (x *extractor) mkdirAll(name string) error {
	if x.dirs[name] {
		return nil
	}
	if parent := path.Dir(name); !x.dirs[parent] {
		if x.seen[parent] {
			return conflict(parent)
		}
		x.seen[parent] = true
		if err := x.mkdirAll(parent); err != nil {
			return err
		}
	}
	err := x.r.Mkdir(name, 0o755)
	if errors.Is(err, fs.ErrExist) {
		return conflict(name)
	}
	if err != nil {
		return err
	}
	x.dirs[name] = true
	return nil
}

// account checks one more file of size bytes against the limits, before it
// is written.
func (x *extractor) account(size int64) error {
	if size < 0 {
		return agentrpc.Invalid("the archive has an entry of negative size")
	}
	x.st.Files++
	x.st.Bytes += size
	if x.st.Files > x.lim.MaxFiles {
		return agentrpc.Invalid("the archive has more than %d files, its limit", x.lim.MaxFiles)
	}
	if x.st.Bytes > x.budget {
		return x.tooBig()
	}
	return nil
}

func (x *extractor) file(name string, size int64, mode fs.FileMode, data io.Reader) error {
	if x.seen[name] {
		return conflict(name)
	}
	x.seen[name] = true
	if err := x.account(size); err != nil {
		return err
	}
	if err := x.mkdirAll(path.Dir(name)); err != nil {
		return err
	}
	f, err := x.r.Create(name, mode)
	if errors.Is(err, fs.ErrExist) {
		return conflict(name)
	}
	if err != nil {
		return err
	}
	_, err = io.CopyN(f, data, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return agentrpc.Invalid("the archive is truncated at %q", name)
	}
	return err
}

// hardlink makes name an independent copy of the regular file the archive
// already wrote at target. Anything else is skipped.
func (x *extractor) hardlink(name, target string) error {
	src, err := memberName(target)
	if err != nil || src == "" || x.seen[name] {
		x.st.Skipped++
		return nil
	}
	fi, err := x.r.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		x.st.Skipped++
		return nil
	}
	in, err := x.r.OpenFile(src, readNoFollow, 0)
	if err != nil {
		x.st.Skipped++
		return nil
	}
	defer in.Close()
	return x.file(name, fi.Size(), fi.Mode().Perm(), in)
}

// link makes one deferred symlink, where its directory is a real one and
// its name is free. It reports whether it did.
func (x *extractor) link(l pendingLink) bool {
	dir := path.Dir(l.name)
	if res, err := x.r.Resolve(dir); err != nil || res != dir {
		return false // a link on the way: the link would land elsewhere
	}
	if err := x.mkdirAll(dir); err != nil {
		return false
	}
	if _, err := x.r.Lstat(l.name); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return x.r.Symlink(l.target, l.name) == nil
}

// dropEscaping removes every symlink under r whose target, followed by the
// kernel beneath r, leaves it, and returns how many it removed. A dangling
// link, a loop or a target through a file points at nothing and stays.
func dropEscaping(r *sitefs.Root) (int, error) {
	var bad []string
	err := r.Walk(".", func(rel string, fi fs.FileInfo) error {
		if fi.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		if _, err := r.Stat(rel); errors.Is(err, sitefs.ErrEscape) {
			bad = append(bad, rel)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, rel := range bad {
		if err := r.Remove(rel); err != nil {
			return 0, err
		}
	}
	return len(bad), nil
}

// capped reads at most left bytes from r; past that it fails and sets over.
type capped struct {
	r    io.Reader
	left int64
	over bool
}

var errStreamCap = errors.New("the archive stream is larger than its limit")

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		c.over = true
		return 0, errStreamCap
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
