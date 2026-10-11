package install

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/cloudground-it/cloudground/internal/install/managed"
	"github.com/cloudground-it/cloudground/internal/units"
)

// The configuration files travel inside the binary, and so do the panel's
// systemd units (internal/units, shared with the release build): a verified
// cgctl is what makes them verified, and nothing is read from a source tree
// at install time.
//
//go:embed templates/*
var assets embed.FS

// Vars is what the templates may depend on.
type Vars struct {
	// QUIC: this nginx has ngx_http_v3_module (`nginx -V`, on stderr).
	QUIC bool
}

// Render returns a managed file's content for v.
func Render(f managed.File, v Vars) ([]byte, error) {
	if f.Template == "" {
		return nil, fmt.Errorf("%s is edited in place, not rendered", f.Path)
	}
	if name, ok := strings.CutPrefix(f.Template, "units/"); ok {
		return Unit(name) // the units are files, not templates
	}
	raw, err := assets.ReadFile(f.Template)
	if err != nil {
		return nil, err
	}
	t, err := template.New(f.Template).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Unit returns an embedded unit file by name ("cloudground-api.socket").
func Unit(name string) ([]byte, error) {
	b, ok := units.Get(name)
	if !ok {
		return nil, fmt.Errorf("no unit %q in internal/units", name)
	}
	return b, nil
}

// The nginx.conf edit (SPEC §3 step 18). These are main/events-level
// settings, which an http-level include in conf.d cannot set.
//
// Only worker_connections is changed here. multi_accept was measured on a
// 2-core box and made no reproducible difference (sustained 9529 vs 9438 rps,
// static 21216 vs 21447 — the runs overlap), so it stays at the distro
// default rather than becoming config we cannot justify.
//
// Kernel TLS offload (ssl_conf_command Options KTLS) is deliberately NOT enabled.
// Another panel ships it, so we implemented it, verified the kernel really was doing
// the encryption (/proc/net/tls_stat TlsTxSw incrementing) — and then measured it
// as a large net LOSS on this workload:
//
//	KTLS on : sustained 6642 rps, static 14246 rps
//	KTLS off: sustained 9177 rps, static 19601 rps
//
// ~28% off cached throughput and ~31% off static, reproduced with multi_accept
// both on and off. KTLS pays for itself on large sequential transfers; a page
// cache serves many small responses, where the per-record kernel transitions
// cost more than the copy it avoids. Left off, with the numbers, so nobody
// "optimises" it back in without re-measuring.
const (
	// TunedMarker is the line that says nginx.conf has been edited.
	TunedMarker = "cloudground-tuned"
	// WorkerConnections replaces Ubuntu's 768 (two workers capped the box
	// at ~1536 connections, well before the cache ran out).
	WorkerConnections = 4096
	// WorkerRlimitNofile: a worker needs a descriptor per connection plus
	// its upstreams and files.
	WorkerRlimitNofile = 65535
)

// RejectedTuning are the settings tried and measured as no win (above); no
// template and no edit may produce them (TestRejectedTuningStaysOut).
var RejectedTuning = []string{"multi_accept", "KTLS", "ssl_conf_command"}

var (
	workerConnections = regexp.MustCompile(`^([[:space:]]*)worker_connections[[:space:]]+[0-9]+;`)
	workerProcesses   = regexp.MustCompile(`^worker_processes`)
)

// TuneNginxConf edits the distribution's nginx.conf: every
// "worker_connections <n>;" becomes 4096 (indentation and the rest of the line
// kept), and the marker and worker_rlimit_nofile go in before the first line
// that starts with worker_processes. It is the edit the sed lines of the old
// installer made; the golden test holds it to their output.
func TuneNginxConf(in []byte) []byte {
	lines := strings.SplitAfter(string(in), "\n")
	var b strings.Builder
	inserted := false
	for _, l := range lines {
		if l == "" {
			continue
		}
		l = workerConnections.ReplaceAllString(l, fmt.Sprintf("${1}worker_connections %d;", WorkerConnections))
		if !inserted && workerProcesses.MatchString(l) {
			fmt.Fprintf(&b, "# %s\nworker_rlimit_nofile %d;\n", TunedMarker, WorkerRlimitNofile)
			inserted = true
		}
		b.WriteString(l)
	}
	return []byte(b.String())
}

// SetEnvLine makes text carry key=value: every line that starts with "key="
// is replaced, or the line is appended (after a newline when text does not
// end with one). Other lines are kept byte for byte.
func SetEnvLine(text []byte, key, value string) []byte {
	line := key + "=" + value
	lines := strings.SplitAfter(string(text), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			nl := ""
			if strings.HasSuffix(l, "\n") {
				nl = "\n"
			}
			lines[i] = line + nl
			found = true
		}
	}
	out := strings.Join(lines, "")
	if found {
		return []byte(out)
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out + line + "\n")
}
