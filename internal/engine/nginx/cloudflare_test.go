package nginx

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/cfips"
)

func TestCloudflareInclude(t *testing.T) {
	r, err := cfips.Parse([]string{"104.16.0.0/13"}, []string{"2606:4700::/32"})
	if err != nil {
		t.Fatal(err)
	}
	got := CloudflareInclude(r)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	// The ranges and nothing else: no header name, no recursion.
	if strings.Join(lines, "|") != "set_real_ip_from 104.16.0.0/13;|set_real_ip_from 2606:4700::/32;" {
		t.Fatalf("include:\n%s", got)
	}
	if (Renderer{}).CloudflareIncludeFile() != "/etc/nginx/conf.d/cloudground-cloudflare.inc" || (Renderer{ConfDir: "/x"}).BannedFile() != "/x/cloudground-banned.conf" {
		t.Fatal("file names")
	}
}

func TestBanned(t *testing.T) {
	pfx := func(ss ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, s := range ss {
			out = append(out, netip.MustParsePrefix(s))
		}
		return out
	}
	got := Banned(
		pfx("203.0.113.9/32", "10.1.0.0/16", "198.51.100.0/24", "2001:db8:1::/64", "203.0.113.9/32"),
		pfx("10.0.0.0/8", "198.51.100.7/32"),
	)
	want := "geo $cg_banned {\n" +
		"    default  0;\n" +
		"    10.0.0.0/8  0;\n" + // the block inside it is left out
		"    198.51.100.0/24  1;\n" +
		"    198.51.100.7/32  0;\n" + // longer: wins inside the block
		"    203.0.113.9/32  1;\n" +
		"    2001:db8:1::/64  1;\n" +
		"}\n"
	if !strings.HasSuffix(got, want) || !strings.HasPrefix(got, "# Written by CloudGround") {
		t.Fatalf("banned:\n%s", got)
	}
	if empty := Banned(nil, nil); !strings.HasSuffix(empty, "geo $cg_banned {\n    default  0;\n}\n") {
		t.Fatalf("empty:\n%s", empty)
	}
}
