package migration

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// A member name memberName keeps is relative, clean and below the tree.
func FuzzMemberName(f *testing.F) {
	for _, s := range []string{"a", "./a/b", "a/../b", "../a", "/etc/passwd", "a/../../b", "a\x00b", ".", "", "a//b/", `..\a`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		n, err := memberName(raw)
		if err != nil || n == "" {
			return
		}
		if strings.IndexByte(n, 0) >= 0 || path.IsAbs(n) || path.Clean(n) != n || n == ".." || strings.HasPrefix(n, "../") {
			t.Fatalf("memberName(%q) = %q", raw, n)
		}
		if j := path.Join("/tree", n); !strings.HasPrefix(j, "/tree/") {
			t.Fatalf("memberName(%q) = %q leaves the tree: %s", raw, n, j)
		}
	})
}

// Whatever three members an archive holds, unpacking it writes nothing
// beside the tree, and no link left in the tree leads out of it.
func FuzzUnpack(f *testing.F) {
	f.Add("a", "", uint8(0), "b", "a", uint8(2), "c/d", "../x", uint8(3))
	f.Add("l", "..", uint8(2), "l/x", "", uint8(0), "h", "l/x", uint8(3))
	f.Add("d", "", uint8(1), "d/../../e", "", uint8(0), "s", "/etc", uint8(2))
	f.Add("a/b", "", uint8(0), "a", "b", uint8(2), "a/c", "", uint8(0))
	f.Fuzz(func(t *testing.T, n1, l1 string, k1 uint8, n2, l2 string, k2 uint8, n3, l3 string, k3 uint8) {
		var ms []member
		for _, m := range []struct {
			name, link string
			kind       uint8
		}{{n1, l1, k1}, {n2, l2, k2}, {n3, l3, k3}} {
			if m.name == "" || len(m.name) > 200 || len(m.link) > 200 || strings.IndexByte(m.name+m.link, 0) >= 0 {
				return
			}
			switch m.kind % 4 {
			case 0:
				ms = append(ms, reg(m.name, "x"))
			case 1:
				ms = append(ms, dir(m.name))
			case 2:
				ms = append(ms, symlink(m.name, m.link))
			default:
				ms = append(ms, hardlink(m.name, m.link))
			}
		}
		archive, ok := tgzOrSkip(ms)
		if !ok {
			return
		}
		parent := t.TempDir()
		dest := filepath.Join(parent, "tree")
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		_, _, _ = unpackTo(t, dest, archive, DefaultLimits)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "tree" {
			t.Fatalf("unpacking %v wrote beside the tree: %v", ms, entries)
		}
		_ = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			target, _ := os.Readlink(p)
			if filepath.IsAbs(target) {
				t.Fatalf("absolute link %s -> %s (archive %v)", p, target, ms)
			}
			if j := filepath.Join(filepath.Dir(p), target); j != dest && !strings.HasPrefix(j, dest+"/") {
				t.Fatalf("link %s -> %s leads out of the tree (archive %v)", p, target, ms)
			}
			return nil
		})
	})
}

// tgzOrSkip is tgz for members tar can write; false for one it refuses.
func tgzOrSkip(ms []member) (b []byte, ok bool) {
	defer func() {
		if recover() != nil {
			b, ok = nil, false
		}
	}()
	return tgz(fatalToPanic{}, ms...), true
}

// fatalToPanic lets tgz, written for tests, report a header tar refuses to
// tgzOrSkip, which skips that input.
type fatalToPanic struct{ testing.TB }

func (fatalToPanic) Helper()                           {}
func (fatalToPanic) Fatalf(format string, args ...any) { panic("skip") }
func (fatalToPanic) Fatal(args ...any)                 { panic("skip") }
