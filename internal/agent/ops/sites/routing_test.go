package sites

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

const spa = "location / {\n    try_files $uri $uri/ /index.html;\n}\n"

func TestSetRoutingRendersAndReports(t *testing.T) {
	e := newEnv(t)
	s := e.site(21, "spa.example.com", state.SiteStatic)
	e.create(t, s)
	_, reloads := e.nginx.Counts()
	res, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: strings.ReplaceAll(spa, "\n", "\r\n")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Routing != spa {
		t.Errorf("stored %q", res.Routing)
	}
	vhost, _ := os.ReadFile(e.vhostPath(s))
	if !strings.Contains(string(vhost), "        try_files $uri $uri/ /index.html;\n") {
		t.Errorf("the vhost does not carry the routing:\n%s", vhost)
	}
	if _, after := e.nginx.Counts(); after != reloads+1 {
		t.Errorf("nginx reloaded %d times", after-reloads)
	}
	found := false
	for _, f := range res.Files {
		found = found || f.Path == e.vhostPath(s)
	}
	if !found {
		t.Errorf("the vhost is not among the files reported: %v", res.Files)
	}

	// A regeneration renders the stored block again.
	s.Config.Routing = res.Routing
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(e.vhostPath(s)); string(again) != string(vhost) {
		t.Error("an apply lost the routing")
	}

	view, err := e.Routing(context.Background(), rpc.SiteRef{Site: s})
	if err != nil {
		t.Fatal(err)
	}
	if view.Current != spa || !strings.Contains(view.Default, "/index.html =404") || !strings.Contains(view.Before, "ssl_certificate") || view.After != "}\n" {
		t.Errorf("view %+v", view)
	}

	// The default text, or nothing, is the default: stored as "".
	for _, text := range []string{view.Default, "  \n"} {
		res, err = e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: text})
		if err != nil || res.Routing != "" {
			t.Errorf("restore with %q: %q %v", text, res.Routing, err)
		}
	}
	if b, _ := os.ReadFile(e.vhostPath(s)); strings.Contains(string(b), "/index.html;") {
		t.Error("the default routing is not back")
	}
}

func TestSetRoutingRefusalsLeaveTheSiteAlone(t *testing.T) {
	e := newEnv(t)
	s := e.site(22, "php.example.com", state.SitePHP)
	e.create(t, s)
	before, _ := os.ReadFile(e.vhostPath(s))
	tests, _ := e.nginx.Counts()

	_, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: "location / { include /etc/passwd; }"})
	if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "include") {
		t.Errorf("a refused directive: %v", err)
	}
	if after, _ := e.nginx.Counts(); after != tests {
		t.Error("a refused routing reached nginx")
	}

	e.nginx.TestErr = &host.ConfigError{Output: "2026/10/10 02:10:42 [emerg] 1#1: invalid value \"notatime\" in " +
		e.vhostPath(s) + ":" + itoa(int64(strings.Count(string(before[:strings.LastIndex(string(before), "    location / {")]), "\n")+2)) +
		"\nnginx: configuration file /etc/nginx/nginx.conf test failed\n"}
	_, err = e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: "location / {\n    expires notatime;\n}\n"})
	e.nginx.TestErr = nil
	if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), `invalid value "notatime" (routing line 2)`) {
		t.Errorf("nginx's refusal: %v", err)
	}
	if after, _ := os.ReadFile(e.vhostPath(s)); string(after) != string(before) {
		t.Error("a routing nginx refused stayed in the vhost")
	}

	nosock := e.site(23, "bare.example.com", state.SiteProxy)
	nosock.Config.ProxyUpstream = "http://127.0.0.1:3000"
	e.create(t, nosock)
	if _, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: nosock, Routing: "location / { root /etc; }"}); agentrpc.CodeOf(err) != agentrpc.CodeInvalid {
		t.Errorf("root outside the tree on a proxy site: %v", err)
	}
}

func TestNginxComplaint(t *testing.T) {
	out := "nginx: [emerg] invalid number of arguments in \"try_files\" directive in /v.conf:60\nnginx: configuration file /etc/nginx/nginx.conf test failed"
	for _, c := range []struct {
		start, lines int
		want         string
	}{
		{50, 20, `invalid number of arguments in "try_files" directive (routing line 10)`},
		{50, 5, `invalid number of arguments in "try_files" directive in /v.conf:60`},
		{70, 5, `invalid number of arguments in "try_files" directive in /v.conf:60`},
	} {
		if got := nginxComplaint(out, "/v.conf", c.start, c.lines); got != c.want {
			t.Errorf("%d,%d: %q", c.start, c.lines, got)
		}
	}
	if got := nginxComplaint("something else", "/v.conf", 1, 1); got != "something else" {
		t.Errorf("no emerg line: %q", got)
	}
}

// A system service's port is refused when saved, and a routing saved
// before a service took its port is refused at the next render: the
// machine is read at every render.
func TestRoutingCannotReachSystemServices(t *testing.T) {
	e := newEnv(t)
	s := e.site(31, "reach.example.com", state.SitePHP)
	e.create(t, s)
	_, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: "location /api/ { proxy_pass http://127.0.0.1:7000; }\n"})
	if agentrpc.CodeOf(err) != agentrpc.CodeInvalid || !strings.Contains(err.Error(), "port 7000") {
		t.Fatalf("a system service's port: %v", err)
	}
	res, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: "location /api/ { proxy_pass http://127.0.0.1:7001; }\n"})
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Routing = res.Routing
	before, _ := os.ReadFile(e.vhostPath(s))
	e.Ops.SystemPorts = func() []int { return []int{7000, 7001} }
	if _, err := e.Apply(context.Background(), rpc.SiteRef{Site: s}); err == nil || !strings.Contains(err.Error(), "port 7001") {
		t.Fatalf("a render after the port was taken: %v", err)
	}
	if after, _ := os.ReadFile(e.vhostPath(s)); string(after) != string(before) {
		t.Error("the refused render changed the vhost")
	}
	// A stored block that no longer passes can still be replaced.
	if _, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: ""}); err != nil {
		t.Fatalf("restore over a stale block: %v", err)
	}
}

// nginx -t passes but the reload fails: every file goes back, and nothing
// says the routing was applied.
func TestSetRoutingReloadFailureRestores(t *testing.T) {
	e := newEnv(t)
	s := e.site(32, "reload.example.com", state.SiteStatic)
	e.create(t, s)
	before, _ := os.ReadFile(e.vhostPath(s))
	e.nginx.ReloadErr = errors.New("no new worker generation")
	_, err := e.SetRouting(context.Background(), rpc.RoutingParams{Site: s, Routing: spa})
	e.nginx.ReloadErr = nil
	if err == nil {
		t.Fatal("a failed reload reported success")
	}
	if after, _ := os.ReadFile(e.vhostPath(s)); string(after) != string(before) {
		t.Error("the vhost kept the routing whose reload failed")
	}
}

func TestListeningPorts(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(proc+"/net", 0o755); err != nil {
		t.Fatal(err)
	}
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000   110        0 1 1\n" + // 3306 mysql
		"   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2001        0 2 1\n" + // 3000 a site
		"   2: 0100007F:1F90 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 3 1\n" + // 8080 established
		"   3: 00000000:1B58 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1\n" // 7000 root
	tcp6 := "  sl  local_address remote_address st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000000000000000000001000000:18EB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   112        0 5 1\n" // 6379 redis
	_ = os.WriteFile(proc+"/net/tcp", []byte(tcp), 0o644)
	_ = os.WriteFile(proc+"/net/tcp6", []byte(tcp6), 0o644)
	got := listeningPorts(proc, ownedBy(2001))
	if fmt.Sprint(got) != "[3306 6379 7000]" {
		t.Errorf("ports %v", got)
	}
	// Another site's listener (uid 2002, its app or its shell's) is that
	// site's, reserved for every other site (R-10.5).
	tcp += "   4: 0100007F:1F41 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2002        0 6 1\n" // 8001 site B
	_ = os.WriteFile(proc+"/net/tcp", []byte(tcp), 0o644)
	if got := listeningPorts(proc, ownedBy(2001)); fmt.Sprint(got) != "[3306 6379 7000 8001]" {
		t.Errorf("site A's reserved ports %v: B's 8001 must be among them", got)
	}
	if got := listeningPorts(proc, ownedBy(2002)); fmt.Sprint(got) != "[3000 3306 6379 7000]" {
		t.Errorf("site B's reserved ports %v: A's 3000 must be among them", got)
	}
	if got := listeningPorts(proc, ownedBy(-1)); fmt.Sprint(got) != "[3000 3306 6379 7000 8001]" {
		t.Errorf("an unknown owner reserves every port: %v", got)
	}
}
