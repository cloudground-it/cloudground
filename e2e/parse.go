// Package e2e is CloudGround's end-to-end suite: a Go port of
// scripts/e2e-verify.sh that runs ON a CloudGround server, as root.
//
// The suite itself is behind the `e2e` build tag (see e2e/README.md), so
// `go test ./...` and `go vet ./...` only see this file and its unit tests:
// the pure parsers the suite uses to read command output, kept here so they
// are tested on every build without a server.
package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ParseEnvFile reads KEY=VALUE lines (as in /root/cloudground-admin.env):
// blank lines and # comments are skipped, a leading "export " is dropped and
// one pair of matching single or double quotes around the value is removed.
func ParseEnvFile(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

var phpFPMName = regexp.MustCompile(`^php-fpm(8\.[0-9])$`)

// PickPHPVersion returns the newest PHP 8.x among FPM binaries such as
// /usr/sbin/php-fpm8.4, or "" when there is none. Only the binaries count:
// /etc/php/<ver>/ is created by rendering a pool, so it can exist for a
// version that was never installed. paths need not be sorted.
func PickPHPVersion(paths []string) string {
	var vers []string
	for _, p := range paths {
		if m := phpFPMName.FindStringSubmatch(filepath.Base(p)); m != nil {
			vers = append(vers, m[1])
		}
	}
	if len(vers) == 0 {
		return ""
	}
	sort.Strings(vers) // 8.1 … 8.9: one digit, so lexical order is version order
	return vers[len(vers)-1]
}

// SSHDPort is the first "port N" line of `sshd -T`, or def.
func SSHDPort(sshdT, def string) string {
	for _, line := range strings.Split(sshdT, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "port" {
			return f[1]
		}
	}
	return def
}

var internalKeyRe = regexp.MustCompile(`"([0-9a-f]{32})"  off;`)

// StagingInternalKey extracts the internal bypass key from a staging vhost
// (the first `"<32 hex>"  off;` map entry), or "".
func StagingInternalKey(vhost string) string {
	if m := internalKeyRe.FindStringSubmatch(vhost); m != nil {
		return m[1]
	}
	return ""
}

// ResticSummarySnapshot returns the snapshot_id of the summary message in
// the JSON-lines output of `restic backup --json`, or "".
func ResticSummarySnapshot(out string) string {
	for _, line := range strings.Split(out, "\n") {
		var m struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.MessageType == "summary" {
			return m.SnapshotID
		}
	}
	return ""
}

// ResticSnapshotIDs lists the ids in `restic snapshots --json` output.
func ResticSnapshotIDs(out string) ([]string, error) {
	var snaps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(snaps))
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// HasSnapshot reports whether id (full or short) is the prefix of one of ids,
// the way the bash suite's `case " $(rsnaps)" in *" $1"*` matched.
func HasSnapshot(ids []string, id string) bool {
	if id == "" {
		return false
	}
	for _, s := range ids {
		if strings.HasPrefix(s, id) {
			return true
		}
	}
	return false
}

var wpDBNameRe = regexp.MustCompile(`define\(\s*'DB_NAME',\s*'([^']+)`)

// WPConfigDBName is DB_NAME from a wp-config.php, or "".
func WPConfigDBName(config string) string {
	if m := wpDBNameRe.FindStringSubmatch(config); m != nil {
		return m[1]
	}
	return ""
}

// WPConfigValue is the string a wp-config.php defines name as, with the
// escaping wp-cli writes (\' and \\) undone, or "" when it defines none.
func WPConfigValue(config, name string) string {
	re := regexp.MustCompile(`define\(\s*'` + regexp.QuoteMeta(name) + `',\s*'((?:[^'\\]|\\.)*)'`)
	m := re.FindStringSubmatch(config)
	if m == nil {
		return ""
	}
	return strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(m[1])
}

// QuoteAll percent-encodes everything but RFC 3986 unreserved characters,
// like Python's urllib.parse.quote(s, safe=”).
func QuoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)|0x100, 16)[1:]))
	}
	return b.String()
}

var cacheStatusRe = regexp.MustCompile(`^[A-Za-z]*`)

// CacheStatus is the leading word of an X-CloudGround-Cache value ("HIT",
// "MISS", "BYPASS", …), or "" when the header is absent.
func CacheStatus(h http.Header) string {
	return cacheStatusRe.FindString(strings.TrimSpace(h.Get("X-Cloudground-Cache")))
}

var fatalRe = regexp.MustCompile(`(?i)critical error|Fatal error|There has been a critical`)

// HasPHPFatal reports whether a page body shows a PHP or WordPress fatal.
func HasPHPFatal(body []byte) bool { return fatalRe.Match(body) }

// SetCookieLine returns the first Set-Cookie value that sets cookie name, or "".
func SetCookieLine(h http.Header, name string) string {
	for _, v := range h.Values("Set-Cookie") {
		if strings.HasPrefix(strings.ToLower(v), strings.ToLower(name)) {
			return v
		}
	}
	return ""
}

// URLPath strips "scheme://host" from an absolute URL, keeping path, query
// and fragment ("/" when nothing follows the host).
func URLPath(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:]
		}
		return "/"
	}
	return u
}

// warmerAgent is the user agent of the agent's cache warmer.
const warmerAgent = `"CloudGround-CacheWarmer/`

var requestLineRe = regexp.MustCompile(`"[A-Z]+ (\S+) HTTP/[0-9.]+"`)

// WarmerRequests counts, per request path (with its query), the requests the
// cache warmer made in an access log in the panel's `cloudground` format
// (`… "$request" … "$http_user_agent"`).
func WarmerRequests(log string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(log, "\n") {
		if !strings.HasSuffix(strings.TrimSpace(line), `"`) || !strings.Contains(line, warmerAgent) {
			continue
		}
		if m := requestLineRe.FindStringSubmatch(line); m != nil {
			out[m[1]]++
		}
	}
	return out
}

// CountLines counts the lines of out that match re.
func CountLines(out string, re *regexp.Regexp) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

// CountNonEmptyLines counts lines with any content (what `| wc -l` counts
// for command output that ends in a newline).
func CountNonEmptyLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			n++
		}
	}
	return n
}

// PyBool renders a bool the way the bash suite's python3 helper printed it,
// so failure messages read the same in both suites.
func PyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// DashIP turns 203.0.113.7 into 203-0-113-7, the sslip.io label form.
func DashIP(ip string) string { return strings.ReplaceAll(ip, ".", "-") }

// IsPublicIPv4 reports whether ip is a globally routable IPv4 address (not
// private, loopback, link-local, CGNAT 100.64/10 or unspecified).
func IsPublicIPv4(ip string) bool {
	p := net.ParseIP(ip).To4()
	if p == nil || !p.IsGlobalUnicast() || p.IsPrivate() {
		return false
	}
	return !(p[0] == 100 && p[1]&0xc0 == 64)
}

// SlashKey returns an ssh-ed25519 authorized_keys line (no private key
// exists for it) whose SHA-256 fingerprint contains a "/": a character on
// which the standard base64 ssh-keygen prints and the URL-safe alphabet
// differ, and one a DELETE URL must carry intact.
func SlashKey(comment string) string {
	for seed := 0; ; seed++ {
		var blob []byte
		blob = binary.BigEndian.AppendUint32(blob, 11)
		blob = append(blob, "ssh-ed25519"...)
		blob = binary.BigEndian.AppendUint32(blob, 32)
		for i := 0; i < 32; i++ {
			blob = append(blob, byte(seed+i*7))
		}
		sum := sha256.Sum256(blob)
		if strings.Contains(base64.RawStdEncoding.EncodeToString(sum[:]), "/") {
			return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
		}
	}
}

// KeygenFingerprint reads the fingerprint from `ssh-keygen -lf` output
// ("256 SHA256:… comment (ED25519)").
func KeygenFingerprint(out string) string {
	f := strings.Fields(out)
	if len(f) < 2 || !strings.HasPrefix(f[1], "SHA256:") {
		return ""
	}
	return f[1]
}

// ShadowPassword returns user's password field from /etc/shadow's text.
func ShadowPassword(shadow, user string) (string, bool) {
	for _, line := range strings.Split(shadow, "\n") {
		f := strings.Split(line, ":")
		if len(f) > 1 && f[0] == user {
			return f[1], true
		}
	}
	return "", false
}

// GateForced is the ForceCommand of the shell logins' sshd block
// (siteshell.Forced): sshd runs "cg-site-shell -c cloudground-site-shell"
// with the client's command in SSH_ORIGINAL_COMMAND.
const GateForced = "cloudground-site-shell"

// IsGateClient reports whether a process, from its /proc cmdline and
// environ (NUL-separated), is the shell gate's client running command:
// argv[0] cg-site-shell (or -cg-site-shell, with or without a directory),
// then "-c" and the forced command with command in SSH_ORIGINAL_COMMAND.
func IsGateClient(cmdline, environ []byte, command string) bool {
	argv := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(argv) != 3 || strings.TrimPrefix(filepath.Base(argv[0]), "-") != "cg-site-shell" || argv[1] != "-c" || argv[2] != GateForced {
		return false
	}
	for _, kv := range strings.Split(string(environ), "\x00") {
		if kv == "SSH_ORIGINAL_COMMAND="+command {
			return true
		}
	}
	return false
}
