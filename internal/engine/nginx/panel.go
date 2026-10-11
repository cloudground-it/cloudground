package nginx

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PanelVhostName is the file, in the sites directory, that serves the panel
// on its own domain.
const PanelVhostName = "cloudground-panel.conf"

// PanelInput is what the panel's vhost is rendered from.
type PanelInput struct {
	Domain      string
	ACMEWebroot string
	CertFile    string // the panel certificate (full chain)
	KeyFile     string
	// AccessInclude is the operator's own allow/deny file. It is included
	// with a trailing "*" so nginx starts when it does not exist.
	AccessInclude string
	APISocket     string // the API's Unix socket
	// HTTP2Directive renders "http2 on;" (nginx >= 1.25.1) instead of the
	// listen parameter.
	HTTP2Directive bool
}

// PanelVhost renders the panel's vhost: ACME challenges and a redirect on
// :80, and on :443 the panel certificate with every request proxied to the
// API's socket.
func PanelVhost(in PanelInput) (string, error) {
	if err := ValidateDomain(in.Domain); err != nil {
		return "", err
	}
	for name, p := range map[string]string{
		"ACME webroot": in.ACMEWebroot, "certificate": in.CertFile, "key": in.KeyFile,
		"access include": in.AccessInclude, "API socket": in.APISocket,
	} {
		if !safeConfPath(p) {
			return "", fmt.Errorf("panel vhost: %s path %q is not a clean absolute path", name, p)
		}
	}
	h2Listen, h2Line := " http2", ""
	if in.HTTP2Directive {
		h2Listen, h2Line = "", "\n    http2 on;"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by CloudGround: the panel on %[1]s. Rewritten whenever the
# panel certificate is issued; put access rules in %[5]s.
server {
    listen 80;
    listen [::]:80;
    server_name %[1]s;
    server_tokens off;
    location /.well-known/acme-challenge/ { root %[2]s; }
    location / { return 301 https://$host$request_uri; }
}
server {
    listen 443 ssl%[7]s;
    listen [::]:443 ssl%[7]s;%[8]s
    server_name %[1]s;
    server_tokens off;
    ssl_certificate %[3]s;
    ssl_certificate_key %[4]s;
    add_header Strict-Transport-Security "max-age=31536000" always;
    # Operator access rules live in their own file so a re-render keeps
    # them; the wildcard lets nginx start without one.
    include %[5]s*;
    # The API's per-file transfer limit (nginx's 1 MB default answered 413).
    client_max_body_size 16m;
    location / {
        proxy_pass http://unix:%[6]s;
        proxy_buffering off;
        proxy_read_timeout 1h;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
`, in.Domain, in.ACMEWebroot, in.CertFile, in.KeyFile, in.AccessInclude, in.APISocket, h2Listen, h2Line)
	return b.String(), nil
}

// safeConfPath admits an absolute, clean path made of characters that need
// no quoting in nginx's configuration.
func safeConfPath(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p && confPathRe.MatchString(p)
}

var confPathRe = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)

var buildVersionRe = regexp.MustCompile(`nginx/(\d+)\.(\d+)\.(\d+)`)

// BuildVersion reads nginx's version from `nginx -V` output.
func BuildVersion(buildInfo string) ([3]int, bool) {
	var v [3]int
	m := buildVersionRe.FindStringSubmatch(buildInfo)
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// HasHTTP2Directive reports whether this nginx takes "http2 on;": 1.25.1
// deprecated the listen parameter and warns about it on every test. An
// unreadable version answers false, the form every nginx accepts.
func HasHTTP2Directive(buildInfo string) bool {
	v, ok := BuildVersion(buildInfo)
	if !ok {
		return false
	}
	want := [3]int{1, 25, 1}
	for i := range v {
		if v[i] != want[i] {
			return v[i] > want[i]
		}
	}
	return true
}
