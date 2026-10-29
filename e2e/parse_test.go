package e2e

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/siteshell"
)

func TestSlashKey(t *testing.T) {
	f := strings.Fields(SlashKey("cg-e2e-fp"))
	if len(f) != 3 || f[0] != "ssh-ed25519" || f[2] != "cg-e2e-fp" {
		t.Fatalf("line %q", f)
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) != 51 {
		t.Fatalf("blob %d bytes, %v", len(blob), err)
	}
	sum := sha256.Sum256(blob)
	std := base64.RawStdEncoding.EncodeToString(sum[:])
	if !strings.Contains(std, "/") || std == base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s has no /", std)
	}
}

func TestKeygenFingerprintAndShadow(t *testing.T) {
	if got := KeygenFingerprint("256 SHA256:ab/c+d cg-e2e-fp (ED25519)\n"); got != "SHA256:ab/c+d" {
		t.Errorf("KeygenFingerprint = %q", got)
	}
	if got := KeygenFingerprint("k.pub is not a public key file.\n"); got != "" {
		t.Errorf("KeygenFingerprint of an error = %q", got)
	}
	shadow := "root:*:1::::::\ncg-site-1::20000:0:99999:7:::\ncg-site-2:!:20000::::::\n"
	if pw, ok := ShadowPassword(shadow, "cg-site-1"); !ok || pw != "" {
		t.Errorf("cg-site-1: %q %v", pw, ok)
	}
	if pw, ok := ShadowPassword(shadow, "cg-site-2"); !ok || pw != "!" {
		t.Errorf("cg-site-2: %q %v", pw, ok)
	}
	if _, ok := ShadowPassword(shadow, "cg-site-3"); ok {
		t.Error("a missing user was found")
	}
}

func TestWarmerRequests(t *testing.T) {
	log := `127.0.0.1 - a.test [09/Oct/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 512 0.010 cache:MISS "CloudGround-CacheWarmer/1.0"
127.0.0.1 - a.test [09/Oct/2026:10:00:00 +0000] "GET /sample-page/ HTTP/2.0" 200 512 0.010 cache:MISS "CloudGround-CacheWarmer/1.0"
127.0.0.1 - a.test [09/Oct/2026:10:00:01 +0000] "GET /sample-page/ HTTP/1.1" 200 512 0.001 cache:HIT "CloudGround-CacheWarmer/1.0"
127.0.0.1 - a.test [09/Oct/2026:10:00:01 +0000] "GET /q/?a=1&b=2 HTTP/1.1" 200 512 0.001 cache:HIT "CloudGround-CacheWarmer/1.0"
127.0.0.1 - a.test [09/Oct/2026:10:00:02 +0000] "GET /sample-page/ HTTP/1.1" 200 512 0.001 cache:HIT "curl/8.5.0"
127.0.0.1 - a.test [09/Oct/2026:10:00:02 +0000] "GET /partial/ HTTP/1.1" 200 512 0.001 cache:HIT "CloudGround-CacheWa`
	got := WarmerRequests(log)
	want := map[string]int{"/": 1, "/sample-page/": 2, "/q/?a=1&b=2": 1}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d, want %d", k, got[k], v)
		}
	}
}

func TestParseEnvFile(t *testing.T) {
	got := ParseEnvFile("# admin\nexport IP=203.0.113.7\nPANEL_EMAIL='a@b.c'\nPANEL_PW=\"p=w d\"\n\nbroken line\n")
	want := map[string]string{"IP": "203.0.113.7", "PANEL_EMAIL": "a@b.c", "PANEL_PW": "p=w d"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestPickPHPVersion(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"/usr/sbin/php-fpm8.5", "/usr/sbin/php-fpm8.4"}, "8.5"},
		{[]string{"/usr/sbin/php-fpm8.4"}, "8.4"},
		// not a version binary: php-fpm8.4.bak, php-fpm8.10 (outside the bash glob's ^8\.[0-9]$)
		{[]string{"/usr/sbin/php-fpm8.4.bak", "/usr/sbin/php-fpm8.10", "/usr/sbin/php-fpm8.3"}, "8.3"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := PickPHPVersion(c.in); got != c.want {
			t.Errorf("PickPHPVersion(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSSHDPort(t *testing.T) {
	if got := SSHDPort("permitrootlogin no\nport 2222\nport 22\n", "22"); got != "2222" {
		t.Errorf("got %q", got)
	}
	if got := SSHDPort("", "22"); got != "22" {
		t.Errorf("default: got %q", got)
	}
	if got := SSHDPort("passwordauthentication no\n", "22"); got != "22" {
		t.Errorf("no port line: got %q", got)
	}
}

func TestStagingInternalKey(t *testing.T) {
	v := "map $http_x_cloudground_internal $cg_auth {\n    default \"Staging\";\n    \"0123456789abcdef0123456789abcdef\"  off;\n}\n"
	if got := StagingInternalKey(v); got != "0123456789abcdef0123456789abcdef" {
		t.Errorf("got %q", got)
	}
	if got := StagingInternalKey("no key"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestResticParsers(t *testing.T) {
	out := `{"message_type":"status","percent_done":0.5}
not json
{"message_type":"summary","snapshot_id":"abcdef0123456789"}
`
	if got := ResticSummarySnapshot(out); got != "abcdef0123456789" {
		t.Errorf("summary = %q", got)
	}
	ids, err := ResticSnapshotIDs(`[{"id":"abcdef0123456789","short_id":"abcdef01"},{"id":"99"}]`)
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	if !HasSnapshot(ids, "abcdef01") || !HasSnapshot(ids, "abcdef0123456789") {
		t.Error("prefix match failed")
	}
	if HasSnapshot(ids, "bcdef") || HasSnapshot(ids, "") {
		t.Error("matched a non-prefix or an empty id")
	}
	if _, err := ResticSnapshotIDs("garbage"); err == nil {
		t.Error("garbage parsed")
	}
}

func TestWPConfigDBName(t *testing.T) {
	if got := WPConfigDBName("<?php\ndefine( 'DB_NAME', 'cg_shop' );\n"); got != "cg_shop" {
		t.Errorf("got %q", got)
	}
	if got := WPConfigDBName("define('DB_NAME','x1');"); got != "x1" {
		t.Errorf("got %q", got)
	}
}

func TestWPConfigValue(t *testing.T) {
	cfg := "<?php\ndefine( 'DB_USER', 'blog_admin' );\ndefine( 'DB_PASSWORD', 'a b\\'c\\\\d' );\n"
	if got := WPConfigValue(cfg, "DB_USER"); got != "blog_admin" {
		t.Errorf("DB_USER = %q", got)
	}
	if got := WPConfigValue(cfg, "DB_PASSWORD"); got != `a b'c\d` {
		t.Errorf("DB_PASSWORD = %q", got)
	}
	if got := WPConfigValue(cfg, "DB_HOST"); got != "" {
		t.Errorf("DB_HOST = %q", got)
	}
}

func TestQuoteAll(t *testing.T) {
	if got := QuoteAll("SHA256:ab+c/d="); got != "SHA256%3Aab%2Bc%2Fd%3D" {
		t.Errorf("got %q", got)
	}
	if got := QuoteAll("a-b_c.d~"); got != "a-b_c.d~" {
		t.Errorf("got %q", got)
	}
}

func TestCacheStatus(t *testing.T) {
	h := http.Header{}
	if CacheStatus(h) != "" {
		t.Error("absent header")
	}
	h.Set("X-CloudGround-Cache", "HIT")
	if CacheStatus(h) != "HIT" {
		t.Error("HIT")
	}
	h.Set("X-CloudGround-Cache", "BYPASS (query)")
	if CacheStatus(h) != "BYPASS" {
		t.Error("BYPASS")
	}
}

func TestHasPHPFatal(t *testing.T) {
	if !HasPHPFatal([]byte("<p>There has been a critical error on this website.</p>")) {
		t.Error("WordPress critical error")
	}
	if !HasPHPFatal([]byte("PHP FATAL ERROR: x")) {
		t.Error("case-insensitive")
	}
	if HasPHPFatal([]byte("<html>ok</html>")) {
		t.Error("false positive")
	}
}

func TestSetCookieLine(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "other=1")
	h.Add("Set-Cookie", "cg_e2e_session=ab12; path=/")
	if got := SetCookieLine(h, "cg_e2e_session"); got != "cg_e2e_session=ab12; path=/" {
		t.Errorf("got %q", got)
	}
	if SetCookieLine(h, "nope") != "" {
		t.Error("matched a missing cookie")
	}
}

func TestURLPath(t *testing.T) {
	cases := map[string]string{
		"https://shop.example/cg-db-abc/?x=1": "/cg-db-abc/?x=1",
		"https://shop.example":                "/",
		"/already":                            "/already",
	}
	for in, want := range cases {
		if got := URLPath(in); got != want {
			t.Errorf("URLPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountLines(t *testing.T) {
	re := regexp.MustCompile(`^(Reloading|Reloaded|Stopping|Stopped|Starting|Started) `)
	out := "Reloading cg.\nReloaded cg.\nsomething else\nStarted x\nStartedx\n"
	if got := CountLines(out, re); got != 3 {
		t.Errorf("got %d", got)
	}
	if got := CountNonEmptyLines("a\nb\n"); got != 2 {
		t.Errorf("non-empty got %d", got)
	}
	if got := CountNonEmptyLines(""); got != 0 {
		t.Errorf("empty got %d", got)
	}
}

func TestJar(t *testing.T) {
	j := NewJar()
	j.SetCookies(nil, []*http.Cookie{{Name: "__Host-cg", Value: "tok", Secure: true}, {Name: "b", Value: "1"}})
	if got := j.Cookies(nil); len(got) != 2 || got[0].Name != "__Host-cg" || got[0].Value != "tok" {
		t.Fatalf("got %v", got)
	}
	j.SetCookies(nil, []*http.Cookie{{Name: "__Host-cg", MaxAge: -1}})
	j.SetCookies(nil, []*http.Cookie{{Name: "b", Expires: time.Unix(1, 0)}})
	if j.Len() != 0 {
		t.Errorf("deleted cookies kept: %v", j.Cookies(nil))
	}
}

func TestDashIP(t *testing.T) {
	if DashIP("203.0.113.7") != "203-0-113-7" {
		t.Error(DashIP("203.0.113.7"))
	}
}

func TestIsPublicIPv4(t *testing.T) {
	for ip, want := range map[string]bool{
		"203.0.113.7": true, "8.8.8.8": true,
		"10.0.0.1": false, "192.168.1.2": false, "172.16.0.1": false, "100.64.0.1": false,
		"127.0.0.1": false, "169.254.1.1": false, "0.0.0.0": false, "::1": false, "2001:db8::1": false, "x": false,
	} {
		if got := IsPublicIPv4(ip); got != want {
			t.Errorf("IsPublicIPv4(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestParseEnvFileAdminFormat(t *testing.T) {
	// /root/cloudground-admin.env as the installer box has it: two lines, no IP.
	got := ParseEnvFile("PANEL_EMAIL=admin@example.com\nPANEL_PW=s3cret=x\n")
	if got["PANEL_EMAIL"] != "admin@example.com" || got["PANEL_PW"] != "s3cret=x" || got["IP"] != "" {
		t.Errorf("got %v", got)
	}
}

// The gate client is found by its forced command and the client's own
// command in its environment (sshd's ForceCommand form).
func TestIsGateClient(t *testing.T) {
	if GateForced != siteshell.Forced {
		t.Fatalf("GateForced %q, siteshell.Forced %q", GateForced, siteshell.Forced)
	}
	env := []byte("USER=cg-shell-1\x00SSH_ORIGINAL_COMMAND=sleep 4512341\x00HOME=/x\x00")
	for _, c := range []struct {
		cmdline, env, command string
		want                  bool
	}{
		{"cg-site-shell\x00-c\x00cloudground-site-shell\x00", string(env), "sleep 4512341", true},
		{"/usr/local/bin/cg-site-shell\x00-c\x00cloudground-site-shell\x00", string(env), "sleep 4512341", true},
		{"cg-site-shell\x00-c\x00cloudground-site-shell\x00", string(env), "sleep 4512342", false},
		{"cg-site-shell\x00-c\x00cloudground-site-shell\x00", "USER=x\x00", "sleep 4512341", false},
		{"sleep\x004512341\x00", string(env), "sleep 4512341", false},
		{"bash\x00-c\x00cloudground-site-shell\x00", string(env), "sleep 4512341", false},
	} {
		if got := IsGateClient([]byte(c.cmdline), []byte(c.env), c.command); got != c.want {
			t.Errorf("IsGateClient(%q, %q, %q) = %v, want %v", c.cmdline, c.env, c.command, got, c.want)
		}
	}
}

// socketBindFrom reads `systemctl --version`.
func socketBindFrom(out string) (bool, string) {
	first, _, _ := strings.Cut(out, "\n")
	var v int
	if _, err := fmt.Sscanf(first, "systemd %d", &v); err != nil {
		return false, fmt.Sprintf("systemctl --version is %q", first)
	}
	switch {
	case v < 249:
		return false, fmt.Sprintf("systemd %d is older than 249, which added SocketBindDeny", v)
	case strings.Contains(out, "-BPF_FRAMEWORK") || !strings.Contains(out, "+BPF_FRAMEWORK"):
		return false, fmt.Sprintf("systemd %d is built without the BPF framework (-BPF_FRAMEWORK)", v)
	}
	return true, ""
}

// The e2e asserts the port rule where systemd can enforce it, and says why
// it skips where it cannot (F-6-2).
func TestSocketBindFrom(t *testing.T) {
	v255 := "systemd 255 (255.4-1ubuntu8.18)\n+PAM +AUDIT +SELINUX -BPF_FRAMEWORK -XKBCOMMON +UTMP\n"
	v259 := "systemd 259 (259.9-0ubuntu0.1)\n+PAM +AUDIT +BPF_FRAMEWORK +BTF -XKBCOMMON\n"
	v248 := "systemd 248 (248.3)\n+PAM +BPF_FRAMEWORK\n"
	if ok, why := socketBindFrom(v259); !ok {
		t.Errorf("259 +BPF_FRAMEWORK: %s", why)
	}
	if ok, why := socketBindFrom(v255); ok || !strings.Contains(why, "without the BPF framework") {
		t.Errorf("255 -BPF_FRAMEWORK: %v %q", ok, why)
	}
	if ok, why := socketBindFrom(v248); ok || !strings.Contains(why, "older than 249") {
		t.Errorf("248: %v %q", ok, why)
	}
	if ok, _ := socketBindFrom("junk"); ok {
		t.Error("junk enforces")
	}
}
