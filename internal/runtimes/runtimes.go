// Package runtimes names the runtimes of managed Node.js and Python sites
// and renders what runs them (#28, SPEC.md): the app's systemd unit, its
// environment file and the dependency step's script. It only produces
// text; ops/sites and ops/runtimes write and run it.
package runtimes

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Runtime names.
const (
	Node   = "node"
	Python = "python"
)

// Dir holds the installed Node.js releases, one directory per major
// (ops/runtimes SPEC §2).
var Dir = "/var/lib/cloudground-runtimes"

// EnvDir holds each app's environment file, root's.
var EnvDir = "/etc/cloudground-app"

// NodeMajors are the Node.js majors offered (SPEC §1).
var NodeMajors = []string{"20", "22", "24"}

var (
	nodeVersionRe   = regexp.MustCompile(`^(20|22|24)$`)
	pythonVersionRe = regexp.MustCompile(`^3\.[0-9]{1,2}$`)
	envNameRe       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	healthPathRe    = regexp.MustCompile(`^(/[A-Za-z0-9._~-]*)+$`)
)

// Of is the runtime of a site type; "" for a type that runs none.
func Of(t state.SiteType) string {
	switch t {
	case state.SiteNode:
		return Node
	case state.SitePython:
		return Python
	}
	return ""
}

// Label is a runtime's name for people.
func Label(runtime string) string {
	if runtime == Node {
		return "Node.js"
	}
	return "Python"
}

// ValidVersion checks a version's form (SPEC §1).
func ValidVersion(runtime, v string) bool {
	switch runtime {
	case Node:
		return nodeVersionRe.MatchString(v)
	case Python:
		return pythonVersionRe.MatchString(v)
	}
	return false
}

// DefaultStart is a new site's start command (SPEC §2).
func DefaultStart(runtime string) string {
	if runtime == Node {
		return "node server.js"
	}
	return "python -m gunicorn --bind unix:$PORT --workers 2 app:app"
}

// Limits (SPEC §2).
const (
	MaxStart    = 1024
	MaxVars     = 100
	MaxValue    = 4096
	maxPathLen  = 200
	minPort     = 1024
	maxPortNum  = 65535
	socketName  = "app.sock"
	runDirRoot  = "/run/cloudground/app"
	unitPrefix  = "cloudground-app-"
	logFileName = "app.log"
)

// reservedPorts are the system services' (internal/engine SPEC §7.3).
var reservedPorts = []int{80, 443, 8443, 3306, 6379, 5252, 9080}

// reservedName reports whether an environment name is the unit's own.
func reservedName(n string) bool {
	switch n {
	case "PORT", "HOST", "PATH", "HOME", "USER", "VIRTUAL_ENV", "NODE_OPTIONS":
		return true
	}
	return strings.HasPrefix(n, "CG_") || strings.HasPrefix(n, "LD_")
}

// CheckName refuses an environment variable name (SPEC §2).
func CheckName(n string) error {
	if !envNameRe.MatchString(n) {
		return fmt.Errorf("%q is not a variable name: capital letters, digits and _, not starting with a digit, at most 64", n)
	}
	if reservedName(n) {
		return fmt.Errorf("%s is set by CloudGround and cannot be changed", n)
	}
	return nil
}

// CheckValue refuses a variable's value (SPEC §2).
func CheckValue(name, v string) error {
	if len(v) > MaxValue {
		return fmt.Errorf("%s: a value is at most %d bytes", name, MaxValue)
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return fmt.Errorf("%s: a value is one line", name)
	}
	return nil
}

// CheckEnv checks a whole environment (names and values).
func CheckEnv(env map[string]string) error {
	if len(env) > MaxVars {
		return fmt.Errorf("at most %d variables", MaxVars)
	}
	for _, n := range sortedKeys(env) {
		if err := CheckName(n); err != nil {
			return err
		}
		if err := CheckValue(n, env[n]); err != nil {
			return err
		}
	}
	return nil
}

// CheckApp checks a site's app settings (SPEC §2).
func CheckApp(t state.SiteType, a state.SiteApp) error {
	rt := Of(t)
	if rt == "" {
		return errors.New("not a Node.js or Python site")
	}
	if !ValidVersion(rt, a.Version) {
		return fmt.Errorf("invalid %s version %q", Label(rt), a.Version)
	}
	if strings.TrimSpace(a.Start) == "" || len(a.Start) > MaxStart || strings.ContainsAny(a.Start, "\r\n\x00") {
		return fmt.Errorf("the start command is one line of 1 to %d characters", MaxStart)
	}
	if len(a.Env)+len(a.Secrets) > MaxVars {
		return fmt.Errorf("at most %d variables", MaxVars)
	}
	if err := CheckEnv(a.Env); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, n := range a.Secrets {
		if err := CheckName(n); err != nil {
			return err
		}
		if _, plain := a.Env[n]; plain || seen[n] {
			return fmt.Errorf("%s is listed twice", n)
		}
		seen[n] = true
	}
	if a.Port != 0 && (a.Port < minPort || a.Port > maxPortNum || slices.Contains(reservedPorts, a.Port)) {
		return fmt.Errorf("port %d: 0 for the socket, or 1024 to 65535 and not a system service's", a.Port)
	}
	if a.HealthPath != "" && (len(a.HealthPath) > maxPathLen || !healthPathRe.MatchString(a.HealthPath)) {
		return fmt.Errorf("the health path is a path like /health")
	}
	return nil
}

// UnitName is the app unit of a site user.
func UnitName(user string) string { return unitPrefix + user + ".service" }

// SocketPath is the socket a site user's app listens on.
func SocketPath(user string) string { return filepath.Join(runDirRoot, user, socketName) }

// EnvFile is the environment file of a site user's app.
func EnvFile(user string) string { return filepath.Join(EnvDir, user+".env") }

// LogFile is a site's app log.
func LogFile(domain string) string { return filepath.Join(phpfpm.LogRoot, domain, logFileName) }

// Upstream is where nginx reaches a site's app: unix:<socket> or
// 127.0.0.1:<port> (internal/engine SPEC §4.11).
func Upstream(s state.Site) string {
	if s.Config.App.Port != 0 {
		return "127.0.0.1:" + strconv.Itoa(s.Config.App.Port)
	}
	return "unix:" + SocketPath(s.SystemUser)
}

// UpstreamName is the nginx upstream of a site's app.
func UpstreamName(id int64) string { return "cg_app_" + strconv.FormatInt(id, 10) }

// NodeDir is an installed Node.js major.
func NodeDir(major string) string { return filepath.Join(Dir, Node, major) }

// Python is the distro interpreter of a version.
func PythonBin(version string) string { return "/usr/bin/python" + version }

// Bin is the directory first on the app's PATH (SPEC §1).
func Bin(s state.Site) string {
	if s.Type == state.SiteNode {
		return filepath.Join(NodeDir(s.Config.App.Version), "bin")
	}
	return filepath.Join(s.RootPath, "current", ".venv", "bin")
}

// pathEnv is the PATH of the app and of its dependency step.
func pathEnv(bin string) string { return bin + ":/usr/local/bin:/usr/bin:/bin" }

// header is the first line of every file rendered here.
const header = "# Managed by CloudGround — do not edit. Changes are detected as drift."

// RenderUnit is the app's unit (SPEC §3).
func RenderUnit(s state.Site) (name, content string, err error) {
	rt := Of(s.Type)
	if rt == "" || s.SystemUser == "" || s.RootPath == "" {
		return "", "", fmt.Errorf("site %q is not a managed app", s.Domain)
	}
	if err := CheckApp(s.Type, s.Config.App); err != nil {
		return "", "", err
	}
	u := s.SystemUser
	port := SocketPath(u)
	if s.Config.App.Port != 0 {
		port = strconv.Itoa(s.Config.App.Port)
	}
	var b strings.Builder
	add := func(lines ...string) {
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	add(header,
		"[Unit]",
		fmt.Sprintf("Description=%s app for %s", Label(rt), s.Domain),
		"After=network.target mariadb.service",
		"",
		"[Service]",
		"Type=simple",
		"User="+u,
		"Group="+u,
		"Slice="+u+".slice",
		"WorkingDirectory="+filepath.Join(s.RootPath, "current"),
		"Environment=PATH="+pathEnv(Bin(s)),
		"Environment=HOME=/tmp",
		"Environment=PORT="+port,
	)
	if s.Config.App.Port == 0 {
		add("Environment=CG_SOCKET=" + SocketPath(u))
	}
	if rt == Node {
		add("Environment=NODE_ENV=production")
	} else {
		add("Environment=VIRTUAL_ENV="+filepath.Join(s.RootPath, "current", ".venv"), "Environment=PYTHONUNBUFFERED=1")
	}
	add(
		"EnvironmentFile=-"+EnvFile(u),
		"Environment="+QuoteEnv("CG_START", s.Config.App.Start),
		"ExecStart="+execStart,
		"RuntimeDirectory=cloudground/app/"+u,
		"RuntimeDirectoryMode=0750",
		"# The socket the app makes is its group's too: nginx's workers are in",
		"# every site group, and nobody else is.",
		"UMask=0007",
		"StandardOutput=append:"+LogFile(s.Domain),
		"StandardError=inherit",
		"# An app that exits or crashes is started again, for ever.",
		"Restart=always",
		"RestartSec=2",
		"StartLimitIntervalSec=0",
		"TimeoutStopSec=20",
		"KillMode=mixed",
		"",
		"# The site's own tree and logs and nothing of any other site (the PHP",
		"# master's and every run's confinement, internal/phpfpm).",
	)
	add(phpfpm.RunSandbox(s)...)
	add("", "[Install]", "WantedBy=multi-user.target")
	return UnitName(u), b.String(), nil
}

// QuoteEnv is a value as a double-quoted Environment= assignment:
// backslash and quote escaped (systemd's C-style rules), % doubled (its
// specifiers). Environment= expands no $ (SPEC §5).
func QuoteEnv(name, v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "%", "%%")
	return `"` + name + "=" + v + `"`
}

// execStart runs CG_START through the shell, as typed: eval parses it as a
// command line, exec makes the app the main process. $$ is systemd's $.
const execStart = `/bin/sh -c 'eval "exec $$CG_START"'`

// RenderEnv is the environment file (SPEC §5).
func RenderEnv(env map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by CloudGround: this app's environment. Root's alone.\n")
	for _, n := range sortedKeys(env) {
		v := strings.ReplaceAll(env[n], `\`, `\\`)
		v = strings.ReplaceAll(v, `"`, `\"`)
		// A newer systemd expands $ in an environment file's double quotes,
		// as a shell would: escaped, the value stays as typed.
		v = strings.ReplaceAll(v, "$", `\$`)
		v = strings.ReplaceAll(v, "`", "\\`")
		b.WriteString(n + `="` + v + "\"\n")
	}
	return b.String()
}

// PrepareScript is the dependency step (SPEC §4); it runs in the release.
func PrepareScript(s state.Site) string {
	if s.Type == state.SiteNode {
		return `set -eu
if [ -f package-lock.json ] || [ -f npm-shrinkwrap.json ]; then
  echo "npm ci"
  npm ci --omit=dev --no-audit --no-fund
elif [ -f package.json ]; then
  echo "npm install"
  npm install --omit=dev --no-audit --no-fund
else
  echo "no package.json: nothing to install"
fi
`
	}
	py := PythonBin(s.Config.App.Version)
	return `set -eu
rm -rf .venv
echo "` + py + ` -m venv .venv"
` + py + ` -m venv .venv
if [ -f requirements.txt ]; then
  echo "pip install -r requirements.txt"
  .venv/bin/python -m pip install --no-cache-dir --disable-pip-version-check -r requirements.txt
else
  echo "no requirements.txt: nothing to install"
fi
`
}

// PrepareEnv is the dependency step's environment.
func PrepareEnv(s state.Site) []string {
	bin := Bin(s)
	if s.Type == state.SitePython {
		bin = "/usr/bin"
	}
	return []string{"PATH=" + pathEnv(bin), "HOME=/tmp", "LANG=C.UTF-8", "npm_config_update_notifier=false"}
}

// PickNode is the newest release of a major in nodejs.org's index.json
// (SPEC §6).
func PickNode(index []byte, major string) (string, error) {
	var rel []struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(index, &rel); err != nil {
		return "", fmt.Errorf("Node.js's release index: %w", err)
	}
	best := []int(nil)
	bestV := ""
	for _, r := range rel {
		parts := strings.Split(strings.TrimPrefix(r.Version, "v"), ".")
		if len(parts) != 3 || parts[0] != major || !strings.HasPrefix(r.Version, "v") {
			continue
		}
		n := make([]int, 3)
		ok := true
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil || v < 0 {
				ok = false
				break
			}
			n[i] = v
		}
		if ok && (best == nil || slices.Compare(n, best) > 0) {
			best, bestV = n, r.Version
		}
	}
	if bestV == "" {
		return "", fmt.Errorf("Node.js %s has no release", major)
	}
	return bestV, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TarballName is the release tarball for an architecture (x64, arm64).
func TarballName(version, arch string) string {
	return "node-" + version + "-linux-" + arch + ".tar.gz"
}

// ParseShasums is the digest SHASUMS256.txt gives for file (SPEC §6).
func ParseShasums(text, file string) (string, error) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == file {
			if !shaRe.MatchString(f[0]) {
				return "", fmt.Errorf("malformed digest for %s", file)
			}
			return f[0], nil
		}
	}
	return "", fmt.Errorf("SHASUMS256.txt has no line for %s", file)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
