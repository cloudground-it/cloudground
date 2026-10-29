// Package functions describes standalone functions, Lambda-style Node.js
// and Python handlers (#143, v0.9.0, SPEC.md): their rules, their code, their
// schedule, their user, slice, socket-activated units and sandbox, and the
// shims that run one invocation. It only produces text; ops/functions
// writes and runs it.
package functions

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudground-it/cloudground/internal/cronjob"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/runtimes"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Limits and defaults (SPEC §1, §2).
const (
	MaxFunctions   = 100
	MaxFiles       = 64
	MaxCode        = 1 << 20
	MaxDomains     = 10
	MaxOrigins     = 20
	DefaultMemory  = 128
	DefaultTimeout = 10
	minMemory      = 64
	maxMemory      = 1024
	MaxTimeout     = 300
	KeepReleases   = 10
	MaxConnections = 32
	maxPath        = 255
)

// Where things are (SPEC §2, §3). Variables so that tests can move them.
var (
	// Dir holds the environment files and, beneath, the shims.
	Dir = "/etc/cloudground-fn"
	// Root holds each function's code.
	Root = "/srv/functions"
	// LogDir holds each function's two logs.
	LogDir = "/var/log/cloudground/functions"
	// SocketDir holds each function's socket.
	SocketDir = "/run/cloudground/fn"
)

// InvokeSocket is where the agent reaches nginx to invoke a function
// (internal/engine SPEC §8.2): a unix socket the internal server listens on
// in plain HTTP. nginx makes it 0666, so it lives in the agent's state
// directory, root's 0700 (internal/install SPEC): only root can reach it.
const InvokeSocket = "/var/lib/cloudground-agent/nginx-functions.sock"

// InternalHost is the Host the agent's invocations carry.
const InternalHost = "cloudground-functions"

// ManifestFile lists a release's code files (SPEC §2).
const ManifestFile = ".cg-files"

// ParentSlice holds every function's slice (SPEC §3.1).
const ParentSlice = "cg-fn.slice"

// The certbot lineages (SPEC §1.2).
const DomainLineage = "cloudground-functions"

// FunctionLineage is a function's own domains' lineage.
func FunctionLineage(id int64) string { return "cloudground-fn-" + strconv.FormatInt(id, 10) }

var lineageRe = regexp.MustCompile(`^cloudground-fn-[1-9][0-9]{0,17}$`)

// ValidLineage reports whether name is one of the functions' lineages.
func ValidLineage(name string) bool { return name == DomainLineage || lineageRe.MatchString(name) }

var (
	//go:embed shim/fn.mjs
	ShimNode string
	//go:embed shim/fn.py
	ShimPython string
)

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// ValidName checks a function's name.
func ValidName(n string) bool { return nameRe.MatchString(n) }

// Defaults fills what a new function leaves out.
func Defaults(f state.Function) state.Function {
	if f.MemoryMB == 0 {
		f.MemoryMB = DefaultMemory
	}
	if f.TimeoutSec == 0 {
		f.TimeoutSec = DefaultTimeout
	}
	if f.Version == "" {
		f.Version = "22"
		if f.Runtime == runtimes.Python {
			f.Version = "3.12"
		}
	}
	if f.Auth == "" {
		f.Auth = state.FunctionPublic
	}
	return f
}

// Check refuses a function's settings (SPEC §1).
func Check(f state.Function) error {
	if !ValidName(f.Name) {
		return fmt.Errorf("%q is not a function name: lower-case letters, digits and -, starting with a letter, at most 31", f.Name)
	}
	if f.Runtime != runtimes.Node && f.Runtime != runtimes.Python {
		return fmt.Errorf("the runtime is node or python")
	}
	if !runtimes.ValidVersion(f.Runtime, f.Version) {
		return fmt.Errorf("invalid %s version %q", runtimes.Label(f.Runtime), f.Version)
	}
	if f.MemoryMB < minMemory || f.MemoryMB > maxMemory {
		return fmt.Errorf("memory is %d to %d MB", minMemory, maxMemory)
	}
	if f.TimeoutSec < 1 || f.TimeoutSec > MaxTimeout {
		return fmt.Errorf("the timeout is 1 to %d seconds", MaxTimeout)
	}
	if f.Schedule != "" {
		if err := cronjob.ValidateSchedule(f.Schedule); err != nil {
			return err
		}
	}
	if f.Auth != state.FunctionPublic && f.Auth != state.FunctionToken {
		return fmt.Errorf("auth is public or token")
	}
	if len(f.Env)+len(f.Secrets) > runtimes.MaxVars {
		return fmt.Errorf("at most %d variables", runtimes.MaxVars)
	}
	if err := runtimes.CheckEnv(f.Env); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, n := range f.Secrets {
		if err := runtimes.CheckName(n); err != nil {
			return err
		}
		if _, plain := f.Env[n]; plain || seen[n] {
			return fmt.Errorf("%s is listed twice", n)
		}
		seen[n] = true
	}
	if err := CheckOrigins(f.CORSOrigins); err != nil {
		return err
	}
	if len(f.Domains) > MaxDomains {
		return fmt.Errorf("at most %d domains", MaxDomains)
	}
	return nil
}

var originRe = regexp.MustCompile(`^https?://[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// ValidOrigin reports whether o is a CORS origin a function may list.
func ValidOrigin(o string) bool { return o == "*" || (len(o) <= 300 && originRe.MatchString(o)) }

// CheckOrigins refuses a CORS origin list (SPEC §1).
func CheckOrigins(list []string) error {
	if len(list) > MaxOrigins {
		return fmt.Errorf("at most %d CORS origins", MaxOrigins)
	}
	seen := map[string]bool{}
	for _, o := range list {
		if !ValidOrigin(o) {
			return fmt.Errorf("%q is not an origin: * or http(s)://host[:port], without a path", o)
		}
		if seen[o] {
			return fmt.Errorf("the origin %s is listed twice", o)
		}
		seen[o] = true
	}
	if seen["*"] && len(list) > 1 {
		return fmt.Errorf("* allows every origin: list it alone")
	}
	return nil
}

var partRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// ValidPath reports whether p may name a code file (SPEC §2).
func ValidPath(p string) bool {
	if p == "" || len(p) > maxPath || p == ManifestFile {
		return false
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "." || part == ".." || !partRe.MatchString(part) {
			return false
		}
	}
	return parts[0] != "node_modules" && parts[0] != ".venv"
}

// CheckFiles refuses a release's code (SPEC §2).
func CheckFiles(runtime string, files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("the code is empty")
	}
	if len(files) > MaxFiles {
		return fmt.Errorf("at most %d files", MaxFiles)
	}
	total := 0
	for p, content := range files {
		if !ValidPath(p) {
			return fmt.Errorf("%q is not a file path a function may have", p)
		}
		if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
			return fmt.Errorf("%s must be UTF-8 text", p)
		}
		total += len(content)
	}
	if total > MaxCode {
		return fmt.Errorf("the code is at most %d KiB", MaxCode>>10)
	}
	entry := Entry(runtime)
	if strings.TrimSpace(files[entry]) == "" {
		return fmt.Errorf("the code needs its entry file, %s", entry)
	}
	return nil
}

// SortedPaths are a release's paths, in order.
func SortedPaths(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// NeedsInstall reports whether a release has dependencies to install
// (SPEC §2).
func NeedsInstall(runtime string, files map[string]string) bool {
	if runtime == runtimes.Python {
		_, ok := files["requirements.txt"]
		return ok
	}
	_, ok := files["package.json"]
	return ok
}

// Entry is the entry file of a runtime (SPEC §2).
func Entry(runtime string) string {
	if runtime == runtimes.Python {
		return "main.py"
	}
	return "index.js"
}

// Templates of a new function's code (SPEC §7).
const (
	TemplateHTTP      = "http"
	TemplateScheduled = "scheduled"
)

// Starter is the code a new function gets without any.
func Starter(runtime, template string) map[string]string {
	py := runtime == runtimes.Python
	var code string
	switch {
	case template == TemplateScheduled && py:
		code = `# A CloudGround function run on its schedule: handler(event) runs once each time.
# event.source is "schedule" for a scheduled run, "test" for a test run.
import datetime


def handler(event):
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    print("ran at", now, "from", event["source"], flush=True)
    return {"ran_at": now, "source": event["source"]}
`
	case template == TemplateScheduled:
		code = `// A CloudGround function run on its schedule: handler(event) runs once each time.
// event.source is "schedule" for a scheduled run, "test" for a test run.
exports.handler = async (event) => {
  const now = new Date().toISOString();
  console.log("ran at", now, "from", event.source);
  return { ran_at: now, source: event.source };
};
`
	case py:
		code = `# A CloudGround function: handler(event) answers each request.
# event: method, path, query, headers, body, isBase64Encoded, source.


def handler(event):
    return {
        "statusCode": 200,
        "headers": {"Content-Type": "application/json"},
        "body": {"hello": "from Python", "method": event["method"], "path": event["path"], "source": event["source"]},
    }
`
	default:
		code = `// A CloudGround function: handler(event) answers each request.
// event: method, path, query, headers, body, isBase64Encoded, source.
exports.handler = async (event) => {
  return {
    statusCode: 200,
    headers: { "Content-Type": "application/json" },
    body: { hello: "from Node.js", method: event.method, path: event.path, source: event.source },
  };
};
`
	}
	return map[string]string{Entry(runtime): code}
}

// URLs are a function's public addresses (SPEC §1.2).
func URLs(f state.Function, functionsDomain string) []string {
	out := []string{}
	if functionsDomain != "" {
		out = append(out, "https://"+functionsDomain+"/"+f.Name)
	}
	for _, d := range f.Domains {
		out = append(out, "https://"+d+"/")
	}
	return out
}

const tokenPrefix = "cgf_"

var tokenRe = regexp.MustCompile(`^cgf_[A-Za-z0-9_-]{43}$`)

// NewToken is a new bearer token (SPEC §1.1).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidToken reports whether t has a token's form.
func ValidToken(t string) bool { return tokenRe.MatchString(t) }

func id10(id int64) string { return strconv.FormatInt(id, 10) }

// User is a function's system user and group (SPEC §3).
func User(id int64) string { return "cg-fn-" + id10(id) }

var userRe = regexp.MustCompile(`^cg-fn-[1-9][0-9]{0,17}$`)

// ValidUser reports whether u is a function's user name.
func ValidUser(u string) bool { return userRe.MatchString(u) }

// Slice is a function's slice, a child of ParentSlice.
func Slice(id int64) string { return User(id) + ".slice" }

// UnitBase is a function's unit name without its suffix.
func UnitBase(id int64) string { return "cloudground-fn-" + id10(id) }

// SocketUnit and ServiceTemplate are a function's two unit files.
func SocketUnit(id int64) string      { return UnitBase(id) + ".socket" }
func ServiceTemplate(id int64) string { return UnitBase(id) + "@.service" }

// SocketPath is where nginx reaches a function.
func SocketPath(id int64) string { return filepath.Join(SocketDir, id10(id)+".sock") }

// EnvFile is a function's environment file.
func EnvFile(id int64) string { return filepath.Join(Dir, id10(id)+".env") }

// ShimDir holds the shims.
func ShimDir() string { return filepath.Join(Dir, "shim") }

// AccessLog and OutputLog are a function's logs (SPEC §3).
func AccessLog(name string) string { return filepath.Join(LogDir, name+".access.log") }
func OutputLog(name string) string { return filepath.Join(LogDir, name+".output.log") }

// CodeDir is a function's directory.
func CodeDir(name string) string { return filepath.Join(Root, name) }

// The v0.8.x names, for the migration (ops/functions SPEC §9).

// LegacyUnitBase is a v0.8.x site function's unit name without its suffix.
func LegacyUnitBase(user, name string) string { return "cloudground-fn-" + user + "-" + name }

// LegacyEnvFile is a v0.8.x site function's environment file.
func LegacyEnvFile(user, name string) string { return filepath.Join(Dir, user+"-"+name+".env") }

// LegacyCodeDir is a v0.8.x function's directory in its site's tree,
// relative to it.
func LegacyCodeDir(name string) string { return "functions/" + name }

const header = "# Managed by CloudGround — do not edit. Changes are detected as drift."

// RenderParentSlice is cg-fn.slice (SPEC §3.1).
func RenderParentSlice() string {
	return strings.Join([]string{
		header,
		"[Unit]",
		"Description=CloudGround functions: every function's slice",
		"",
		"[Slice]",
		"CPUAccounting=yes",
		"MemoryAccounting=yes",
		"TasksAccounting=yes",
		"# All the functions together: reclaimed over MemoryHigh, killed at MemoryMax,",
		"# half the CPU's weight of a site when both are busy.",
		"MemoryHigh=40%",
		"MemoryMax=50%",
		"CPUWeight=50",
		"TasksMax=4096",
		"",
	}, "\n")
}

// RenderSocket is the socket unit (SPEC §3.2).
func RenderSocket(f state.Function) string {
	return strings.Join([]string{
		header,
		"[Unit]",
		"Description=Function " + f.Name,
		"",
		"[Socket]",
		"ListenStream=" + SocketPath(f.ID),
		"# nginx's workers reach it, nobody else: not a site, not the function's user.",
		"SocketUser=root",
		"SocketGroup=www-data",
		"SocketMode=0660",
		"DirectoryMode=0755",
		"# One instance per connection: nothing runs between invocations.",
		"Accept=yes",
		"MaxConnections=" + strconv.Itoa(MaxConnections),
		"# The only source is nginx, which bounds each client (internal/engine SPEC §8.4).",
		"MaxConnectionsPerSource=" + strconv.Itoa(MaxConnections),
		"",
		"[Install]",
		"WantedBy=sockets.target",
		"",
	}, "\n")
}

// RenderService is the service template (SPEC §3.2).
func RenderService(f state.Function) string {
	root := filepath.Join(CodeDir(f.Name), "current")
	bin := filepath.Join(runtimes.NodeDir(f.Version), "bin")
	exec := filepath.Join(bin, "node") + " " + filepath.Join(ShimDir(), "fn.mjs")
	if f.Runtime == runtimes.Python {
		bin = "/usr/bin"
		exec = runtimes.PythonBin(f.Version) + " " + filepath.Join(ShimDir(), "fn.py")
	}
	timeoutMS := f.TimeoutSec*1000 - 250
	u := User(f.ID)
	lines := []string{
		header,
		"[Unit]",
		"Description=Function " + f.Name,
		"CollectMode=inactive-or-failed",
		"",
		"[Service]",
		"Type=simple",
		"User=" + u,
		"Group=" + u,
		"Slice=" + Slice(f.ID),
		"WorkingDirectory=" + root,
		"Environment=PATH=" + bin + ":/usr/local/bin:/usr/bin:/bin",
		"Environment=HOME=/tmp",
		"Environment=CG_FUNCTION=" + f.Name,
		"Environment=CG_HANDLER=" + filepath.Join(root, Entry(f.Runtime)),
		"Environment=CG_TIMEOUT_MS=" + strconv.Itoa(timeoutMS),
		"EnvironmentFile=-" + EnvFile(f.ID),
		"ExecStart=" + exec,
		"StandardOutput=append:" + OutputLog(f.Name),
		"StandardError=inherit",
		"# The invocation's limits, enforced by systemd.",
		"RuntimeMaxSec=" + strconv.Itoa(f.TimeoutSec),
		"MemoryMax=" + strconv.Itoa(f.MemoryMB) + "M",
		"MemorySwapMax=0",
		"TasksMax=64",
		"",
		"# Its own code, read-only, and nothing of any site or other function.",
	}
	lines = append(lines, Sandbox(f.Name)...)
	return strings.Join(lines, "\n") + "\n"
}

// shmSize caps a run's own /dev/shm, as a site's (internal/phpfpm).
const shmSize = "64M"

// confinement is the sandbox without its binds (SPEC §3.3).
func confinement() []string {
	return []string{
		"ProtectSystem=strict",
		"TemporaryFileSystem=/srv/sites:ro " + Root + ":ro /var/log/cloudground:ro",
		"TemporaryFileSystem=/dev/shm:size=" + shmSize + ",mode=1777,nosuid,nodev,noexec",
	}
}

// hardening is the rest of the sandbox, the sites' runs' (internal/phpfpm
// Sandbox and RunHardening).
var hardening = []string{
	"InaccessiblePaths=-/etc/nginx -/etc/cloudground -/etc/letsencrypt -/etc/mysql -/var/lib/cloudground -/var/lib/cloudground-agent -/var/cache/cloudground -/var/log/nginx -/var/backups",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"ProtectProc=invisible",
	"ProcSubset=pid",
	"NoNewPrivileges=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectClock=yes",
	"RestrictNamespaces=yes",
	"RestrictRealtime=yes",
	"LockPersonality=yes",
	"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
	// No TCP or UDP port: the call arrives on the socket systemd passes.
	phpfpm.NoPortBinds,
	"CapabilityBoundingSet=",
	"RestrictSUIDSGID=yes",
	"PrivateIPC=yes",
	"SystemCallFilter=@system-service",
	"SystemCallErrorNumber=EPERM",
}

// Sandbox is a run's confinement (SPEC §3.3).
func Sandbox(name string) []string {
	out := append(confinement(), "BindReadOnlyPaths="+CodeDir(name))
	return append(out, hardening...)
}

// InstallSandbox is the dependency step's: the release bound read-write.
func InstallSandbox(name, release string) []string {
	out := append(confinement(), "BindReadOnlyPaths="+CodeDir(name), "BindPaths="+path.Join(CodeDir(name), "releases", release))
	return append(out, hardening...)
}

// PrepareScript is the dependency step (SPEC §2); it runs in the release.
func PrepareScript(runtime, version string) string {
	if runtime == runtimes.Python {
		py := runtimes.PythonBin(version)
		return `set -eu
if [ -f requirements.txt ]; then
  rm -rf .venv
  echo "` + py + ` -m venv .venv"
  ` + py + ` -m venv .venv
  echo "pip install -r requirements.txt"
  .venv/bin/python -m pip install --no-cache-dir --disable-pip-version-check -r requirements.txt
else
  echo "no requirements.txt: nothing to install"
fi
`
	}
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

// PrepareEnv is the dependency step's environment.
func PrepareEnv(runtime, version string) []string {
	bin := filepath.Join(runtimes.NodeDir(version), "bin")
	if runtime == runtimes.Python {
		bin = "/usr/bin"
	}
	return []string{"PATH=" + bin + ":/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "LANG=C.UTF-8", "npm_config_update_notifier=false"}
}

// Due reports whether a five-field schedule fires in t's minute (SPEC §6).
func Due(schedule string, t time.Time) bool {
	f := strings.Fields(schedule)
	if len(f) != 5 || cronjob.ValidateSchedule(schedule) != nil {
		return false
	}
	dow := int(t.Weekday())
	minute := match(f[0], t.Minute(), 0, 59)
	hour := match(f[1], t.Hour(), 0, 23)
	month := match(f[3], int(t.Month()), 1, 12)
	// cron's rule: a day field starting with * does not restrict.
	domRestricted, dowRestricted := !strings.HasPrefix(f[2], "*"), !strings.HasPrefix(f[4], "*")
	dom := match(f[2], t.Day(), 1, 31)
	dw := match(f[4], dow, 0, 7) || (dow == 0 && match(f[4], 7, 0, 7))
	day := dom && dw
	if domRestricted && dowRestricted {
		day = dom || dw
	}
	return minute && hour && month && day
}

// match reports whether v is in a cron field.
func match(field string, v, lo, hi int) bool {
	for _, item := range strings.Split(field, ",") {
		base, stepS, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			step, _ = strconv.Atoi(stepS)
			if step < 1 {
				return false
			}
		}
		a, b := lo, hi
		if base != "*" {
			l, h, isRange := strings.Cut(base, "-")
			a, _ = strconv.Atoi(l)
			b = a
			if isRange {
				b, _ = strconv.Atoi(h)
			}
		}
		if v >= a && v <= b && (v-a)%step == 0 {
			return true
		}
	}
	return false
}
