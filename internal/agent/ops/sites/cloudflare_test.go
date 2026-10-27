package sites

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

func TestCloudflareSiteRendersSharedFiles(t *testing.T) {
	e := newEnv(t)
	include, banned := e.cloudflareFiles()
	ctx := context.Background()

	// Without a site behind Cloudflare, a ban writes nothing and reloads nothing.
	_, before := e.nginx.Counts()
	if err := e.SetBanned(ctx, []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}, nil); err != nil {
		t.Fatal(err)
	}
	if exists(banned) {
		t.Fatal("a ban made the geo with no site behind Cloudflare")
	}
	if _, after := e.nginx.Counts(); after != before {
		t.Fatal("a ban reloaded nginx with no site behind Cloudflare")
	}

	s := e.site(31, "cf.example.com", state.SiteStatic)
	s.Config.Cloudflare = state.SiteCloudflare{RealIP: true}
	res := e.create(t, s)
	vhost, _ := os.ReadFile(e.vhostPath(s))
	if !strings.Contains(string(vhost), "include "+include+";\n    real_ip_header CF-Connecting-IP;\n") {
		t.Fatalf("vhost:\n%s", vhost)
	}
	inc, _ := os.ReadFile(include)
	if !strings.Contains(string(inc), "set_real_ip_from 173.245.48.0/20;\n") || strings.Contains(string(inc), "real_ip_header") {
		t.Fatalf("include from the shipped list:\n%s", inc)
	}
	geo, _ := os.ReadFile(banned)
	if !strings.HasSuffix(string(geo), "geo $cg_banned {\n    default  0;\n}\n") {
		t.Fatalf("empty geo:\n%s", geo)
	}
	// Shared: never one site's managed files.
	for _, f := range res.Files {
		if f.Path == include || f.Path == banned {
			t.Fatalf("%s recorded as the site's", f.Path)
		}
	}

	// Now a ban reaches nginx, once.
	_, before = e.nginx.Counts()
	block := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	for range 2 {
		if err := e.SetBanned(ctx, block, nil); err != nil {
			t.Fatal(err)
		}
	}
	geo, _ = os.ReadFile(banned)
	if !strings.Contains(string(geo), "    203.0.113.9/32  1;\n") {
		t.Fatalf("geo:\n%s", geo)
	}
	if _, after := e.nginx.Counts(); after != before+1 {
		t.Fatalf("%d reloads for one change", after-before)
	}

	// A render keeps what is there: the list is not emptied again.
	if _, err := e.Render(ctx, s); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(banned); string(again) != string(geo) {
		t.Fatal("a render replaced the block list")
	}
}

func TestSetCloudflareRanges(t *testing.T) {
	e := newEnv(t)
	include, _ := e.cloudflareFiles()
	ctx := context.Background()
	p := rpc.CloudflareRangesParams{V4: []string{"104.16.0.0/13"}, V6: []string{"2606:4700::/32"}}
	res, err := e.SetCloudflareRanges(ctx, p)
	if err != nil || !res.Changed {
		t.Fatalf("first: %+v %v", res, err)
	}
	if res, err := e.SetCloudflareRanges(ctx, p); err != nil || res.Changed {
		t.Fatalf("the same list again: %+v %v", res, err)
	}
	inc, _ := os.ReadFile(include)
	if !strings.Contains(string(inc), "set_real_ip_from 104.16.0.0/13;\nset_real_ip_from 2606:4700::/32;\n") {
		t.Fatalf("include:\n%s", inc)
	}

	// Never a range of its own or anyone's choosing.
	for _, bad := range []rpc.CloudflareRangesParams{
		{V4: []string{"0.0.0.0/0"}}, {V4: []string{"127.0.0.0/8"}}, {V4: []string{"104.16.0.0/13"}, V6: []string{"::/0"}},
	} {
		if _, err := e.SetCloudflareRanges(ctx, bad); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
			t.Errorf("%v: %v", bad, err)
		}
	}

	// nginx refuses: the old file is back.
	e.nginx.TestErr = errors.New("nginx: [emerg] test")
	if _, err := e.SetCloudflareRanges(ctx, rpc.CloudflareRangesParams{V4: []string{"172.64.0.0/13"}}); err == nil {
		t.Fatal("a refused configuration was kept")
	}
	if now, _ := os.ReadFile(include); string(now) != string(inc) {
		t.Fatalf("not put back:\n%s", now)
	}
}
