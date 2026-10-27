package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestRetarPutsEverythingUnderFiles(t *testing.T) {
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	for _, e := range []struct {
		name string
		typ  byte
		body string
	}{
		{"/var/www/s.example.com", tar.TypeDir, ""},
		{"/var/www/s.example.com/shared/wp-config.php", tar.TypeReg, "<?php"},
		{"var/www/s.example.com/current", tar.TypeSymlink, ""},
		{"shared/uploads/a.jpg", tar.TypeReg, "jpg"},
	} {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			h.Linkname = "releases/r1"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	if err := retar(tar.NewReader(&in), ow, "/var/www/s.example.com"); err != nil {
		t.Fatal(err)
	}
	ow.Close()
	tr := tar.NewReader(&out)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	want := []string{"files/", "files/shared/wp-config.php", "files/current", "files/shared/uploads/a.jpg"}
	if len(names) != len(want) {
		t.Fatalf("names %q", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names %q, want %q", names, want)
		}
	}

	// An entry that climbs out is refused.
	var bad bytes.Buffer
	bw := tar.NewWriter(&bad)
	bw.WriteHeader(&tar.Header{Name: "../../etc/passwd", Typeflag: tar.TypeReg})
	bw.Close()
	if err := retar(tar.NewReader(&bad), tar.NewWriter(io.Discard), "/var/www/s"); err == nil {
		t.Fatal("an entry leaving files/ was copied")
	}
}

func TestArchiveChunksAndTheirNames(t *testing.T) {
	o := &Ops{ArchiveDir: t.TempDir()}
	site := state.Site{ID: 7, Domain: "s.example.com"}
	dir, err := o.siteArchiveDir(site)
	if err != nil {
		t.Fatal(err)
	}
	name := rpc.ArchiveName("abcdef1234567890", "full")
	if err := os.WriteFile(filepath.Join(dir, name), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := o.ReadArchiveChunk(context.Background(), rpc.ArchiveChunkParams{Site: site, Name: name, Offset: 4, Length: 3})
	if err != nil || string(res.Data) != "456" || res.Size != 10 {
		t.Fatalf("chunk = %q %d %v", res.Data, res.Size, err)
	}
	if _, err := o.ReadArchiveChunk(context.Background(), rpc.ArchiveChunkParams{Site: site, Name: "../../etc/passwd"}); err == nil {
		t.Fatal("a name outside the archives was read")
	}
	if _, err := o.ReadArchiveChunk(context.Background(), rpc.ArchiveChunkParams{Site: state.Site{ID: 8}, Name: name}); err == nil {
		t.Fatal("another site's archive was read")
	}
	if _, err := o.DeleteArchive(context.Background(), rpc.ArchiveChunkParams{Site: site, Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Fatal("the archive survived its deletion")
	}
	// Old archives are swept.
	old := filepath.Join(dir, rpc.ArchiveName("abcdef1234567890", "files"))
	os.WriteFile(old, []byte("x"), 0o600)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)
	o.sweepArchives(time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("a day-old archive was kept")
	}
}

// putBack replaces a file in one rename and a folder whole, leaving the rest
// of the site alone.
func TestPutBackReplacesOnlyThePath(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("sitefs sets owners: run as root (the container suite does)")
	}
	base := t.TempDir()
	srcDir, liveDir := filepath.Join(base, "src"), filepath.Join(base, "live")
	for _, f := range []struct{ path, body string }{
		{"src/shared/wp-config.php", "old-good"},
		{"src/shared/uploads/a.jpg", "a-old"},
		{"live/shared/wp-config.php", "broken"},
		{"live/shared/uploads/a.jpg", "a-new"},
		{"live/shared/uploads/b.jpg", "b-new"},
		{"live/shared/other.txt", "keep"},
	} {
		p := filepath.Join(base, f.path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(f.body), 0o644)
	}
	src, err := sitefs.Open(srcDir, sitefs.Owner{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	live, err := sitefs.Open(liveDir, sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := putBack(src, "shared/wp-config.php", live, "shared/wp-config.php"); err != nil {
		t.Fatal(err)
	}
	if err := putBack(src, "shared/uploads", live, "shared/uploads"); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(liveDir, p))
		return string(b)
	}
	if read("shared/wp-config.php") != "old-good" || read("shared/uploads/a.jpg") != "a-old" || read("shared/other.txt") != "keep" {
		t.Fatal("the paths were not put back")
	}
	if _, err := os.Stat(filepath.Join(liveDir, "shared/uploads/b.jpg")); !os.IsNotExist(err) {
		t.Fatal("a folder was merged, not replaced")
	}
	entries, _ := os.ReadDir(filepath.Join(liveDir, "shared"))
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Fatalf("a temporary was left: %s", e.Name())
		}
	}
}

// statsRestic answers `restic stats` with a size per snapshot ID.
type statsRestic struct {
	host.Restic
	sizes map[string]int64
	calls [][]string
}

func (s *statsRestic) Run(_ context.Context, _ host.Repo, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	var total int64
	for _, a := range args {
		total += s.sizes[a]
	}
	return []byte(fmt.Sprintf("noise\n{\"total_size\":%d,\"total_file_count\":3}\n", total)), nil
}

func TestArchiveRoomIsCheckedFirst(t *testing.T) {
	fake := &statsRestic{sizes: map[string]int64{"files1": 3 << 30, "db1": 1 << 30}}
	free := int64(5 << 30)
	o := &Ops{Restic: fake, FreeSpace: func(string) (int64, error) { return free, nil }}
	repo := host.Repo{URL: "/var/backups/cloudground/r", Password: "pw"}
	parts := []dbPart{{ID: "db1", Name: "wp"}}

	// Files 3 GiB + the dump twice 2 GiB + a tenth: more than 5 GiB.
	err := o.checkArchiveRoom(context.Background(), repo, t.TempDir(), "full", "files1", parts)
	if agentrpc.CodeOf(err) != agentrpc.CodeConflict || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("a full archive that does not fit: %v", err)
	}
	// The files alone, 3 GiB and the 512 MiB margin, fit.
	if err := o.checkArchiveRoom(context.Background(), repo, t.TempDir(), "files", "files1", parts); err != nil {
		t.Fatalf("files only: %v", err)
	}
	// The database alone measures only its parts.
	fake.calls = nil
	if err := o.checkArchiveRoom(context.Background(), repo, t.TempDir(), "database", "files1", parts); err != nil {
		t.Fatalf("database only: %v", err)
	}
	for _, c := range fake.calls {
		if slices.Contains(c, "files1") {
			t.Fatalf("a database archive measured the files: %v", c)
		}
	}
	if got := archiveNeed(100, 0); got != 100+archiveMargin {
		t.Fatalf("the margin's floor: %d", got)
	}
	if got := archiveNeed(10<<30, 0); got != 11<<30 {
		t.Fatalf("a tenth: %d", got)
	}
}
