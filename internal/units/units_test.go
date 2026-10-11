package units

import (
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// Every embedded file is listed in Names and every name is embedded: a unit
// added to the folder but not to Names would ship in no release.
func TestNamesMatchTheEmbeddedFiles(t *testing.T) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		embedded = append(embedded, e.Name())
	}
	got := slices.Sorted(slices.Values(Names))
	if !slices.Equal(embedded, got) {
		t.Fatalf("embedded %v, Names %v", embedded, got)
	}
	for _, n := range Names {
		b, ok := Get(n)
		if !ok || !strings.Contains(string(b), "[Install]") {
			t.Errorf("%s: not a unit file (%d bytes, ok=%v)", n, len(b), ok)
		}
	}
	if _, ok := Get("../units.go"); ok {
		t.Error("Get served a file that is not a unit")
	}
}

func TestRequiredPaths(t *testing.T) {
	got := RequiredPaths([]string{
		"ReadWritePaths=/a -/b +/c",
		"  BindPaths=/d:/e:rbind",
		"TemporaryFileSystem=/f:ro",
		"Environment=X=/nope",
		"# ReadWritePaths=/comment",
	})
	if want := []string{"/a", "/c", "/d", "/f"}; !slices.Equal(got, want) {
		t.Errorf("RequiredPaths = %v, want %v", got, want)
	}
}
