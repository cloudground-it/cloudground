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

// The API lists this machine's addresses (net.InterfaceAddrs, a netlink
// dump): its sandbox must allow AF_NETLINK, or the list is empty, the own
// address checks of the block list and the site rules pass anything, and
// every webhook is refused as "this server's own".
func TestAPIMayListTheMachinesAddresses(t *testing.T) {
	b, ok := Get("cloudground-api.service")
	if !ok {
		t.Fatal("no cloudground-api.service")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "RestrictAddressFamilies="); ok {
			if !slices.Contains(strings.Fields(v), "AF_NETLINK") {
				t.Errorf("the API's RestrictAddressFamilies lacks AF_NETLINK: %s", v)
			}
			return
		}
	}
	t.Error("the API's unit has no RestrictAddressFamilies")
}
