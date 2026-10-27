// Package functions describes a site's functions, Lambda-style Node.js and
// Python handlers (#143, SPEC.md): their rules, their schedule, their
// socket-activated units and the shims that run one invocation. It only
// produces text; ops/functions writes and runs it.
package functions

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"regexp"
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
	MaxPerSite     = 20
	MaxCode        = 256 << 10
	DefaultMemory  = 128
	DefaultTimeout = 10
	minMemory      = 64
	maxMemory      = 1024
	maxTimeout     = 300
	KeepReleases   = 10
	maxConnections = 32
)

// Dir holds the functions' environment files and, beneath, the shims.
var Dir = "/etc/cloudground-fn"

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
func Defaults(f state.SiteFunction) state.SiteFunction {
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
	return f
}

// Check refuses a function's settings (SPEC §1).
func Check(f state.SiteFunction) error {
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
	if f.TimeoutSec < 1 || f.TimeoutSec > maxTimeout {
		return fmt.Errorf("the timeout is 1 to %d seconds", maxTimeout)
	}
	if f.Schedule != "" {
		if err := cronjob.ValidateSchedule(f.Schedule); err != nil {
			return err
		}
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
	return nil
}

// CheckCode refuses code that is too large or not UTF-8 text.
func CheckCode(code string) error {
	if len(code) > MaxCode {
		return fmt.Errorf("the code is at most %d KiB", MaxCode>>10)
	}
	if !utf8.ValidString(code) || strings.ContainsRune(code, 0) {
		return fmt.Errorf("the code must be UTF-8 text")
	}
	return nil
}

// File is the handler file of a runtime (SPEC §2).
func File(runtime string) string {
	if runtime == runtimes.Python {
		return "main.py"
	}
	return "index.js"
}

// Starter is the code a new function gets without any: it echoes the
// request.
func Starter(runtime string) string {
	if runtime == runtimes.Python {
		return `# A CloudGround function: handler(event) answers each request.
# event: method, path, query, headers, body, isBase64Encoded, source.


def handler(event):
    return {
        "statusCode": 200,
        "headers": {"Content-Type": "application/json"},
        "body": {"hello": "from Python", "method": event["method"], "path": event["path"], "source": event["source"]},
    }
`
	}
	return `// A CloudGround function: handler(event) answers each request.
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

// UnitBase is a function's unit name without its suffix.
func UnitBase(user, name string) string { return "cloudground-fn-" + user + "-" + name }

// SocketUnit and ServiceTemplate are a function's two unit files.
func SocketUnit(user, name string) string      { return UnitBase(user, name) + ".socket" }
func ServiceTemplate(user, name string) string { return UnitBase(user, name) + "@.service" }

// SocketPath is where nginx reaches a function.
func SocketPath(user, name string) string {
	return filepath.Join("/run/cloudground/fn", user, name+".sock")
}

// EnvFile is a function's environment file.
func EnvFile(user, name string) string { return filepath.Join(Dir, user+"-"+name+".env") }

// ShimDir holds the shims.
func ShimDir() string { return filepath.Join(Dir, "shim") }

// LogFile is a function's output log.
func LogFile(domain, name string) string {
	return filepath.Join(phpfpm.LogRoot, domain, "fn-"+name+".log")
}

// CodeDir is a function's directory in the site's tree, relative to it.
func CodeDir(name string) string { return "functions/" + name }

const header = "# Managed by CloudGround — do not edit. Changes are detected as drift."

// RenderSocket is the socket unit (SPEC §3).
func RenderSocket(s state.Site, f state.SiteFunction) string {
	return strings.Join([]string{
		header,
		"[Unit]",
		"Description=Function " + f.Name + " of " + s.Domain,
		"",
		"[Socket]",
		"ListenStream=" + SocketPath(s.SystemUser, f.Name),
		"SocketUser=" + s.SystemUser,
		"SocketGroup=" + s.SystemUser,
		"SocketMode=0660",
		"DirectoryMode=0755",
		"# One instance per connection: nothing runs between invocations.",
		"Accept=yes",
		"MaxConnections=" + strconv.Itoa(maxConnections),
		"# The only source is nginx, which bounds each client (internal/engine SPEC §4.13).",
		"MaxConnectionsPerSource=" + strconv.Itoa(maxConnections),
		"",
		"[Install]",
		"WantedBy=sockets.target",
		"",
	}, "\n")
}

// RenderService is the service template (SPEC §3).
func RenderService(s state.Site, f state.SiteFunction) string {
	root := filepath.Join(s.RootPath, CodeDir(f.Name), "current")
	bin := filepath.Join(runtimes.NodeDir(f.Version), "bin")
	exec := filepath.Join(bin, "node") + " " + filepath.Join(ShimDir(), "fn.mjs")
	if f.Runtime == runtimes.Python {
		bin = "/usr/bin"
		exec = runtimes.PythonBin(f.Version) + " " + filepath.Join(ShimDir(), "fn.py")
	}
	timeoutMS := f.TimeoutSec*1000 - 250
	lines := []string{
		header,
		"[Unit]",
		"Description=Function " + f.Name + " of " + s.Domain,
		"CollectMode=inactive-or-failed",
		"",
		"[Service]",
		"Type=simple",
		"User=" + s.SystemUser,
		"Group=" + s.SystemUser,
		"Slice=" + s.SystemUser + ".slice",
		"WorkingDirectory=" + root,
		"Environment=PATH=" + bin + ":/usr/local/bin:/usr/bin:/bin",
		"Environment=HOME=/tmp",
		"Environment=CG_FUNCTION=" + f.Name,
		"Environment=CG_HANDLER=" + filepath.Join(root, File(f.Runtime)),
		"Environment=CG_TIMEOUT_MS=" + strconv.Itoa(timeoutMS),
		"EnvironmentFile=-" + EnvFile(s.SystemUser, f.Name),
		"ExecStart=" + exec,
		"StandardOutput=append:" + LogFile(s.Domain, f.Name),
		"StandardError=inherit",
		"# The invocation's limits, enforced by systemd.",
		"RuntimeMaxSec=" + strconv.Itoa(f.TimeoutSec),
		"MemoryMax=" + strconv.Itoa(f.MemoryMB) + "M",
		"MemorySwapMax=0",
		"TasksMax=64",
		"",
	}
	lines = append(lines, phpfpm.RunSandbox(s)...)
	return strings.Join(lines, "\n") + "\n"
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
