package install

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PlanResult is one step's check.
type PlanResult struct {
	ID     string
	Needed bool
	Why    string
}

// Plan runs every check and changes nothing. A needed step counts as done
// for the checks after it (an upgrade's restart follows replaced binaries).
func Plan(ctx context.Context, steps []Step) []PlanResult {
	res := make([]PlanResult, 0, len(steps))
	for _, s := range steps {
		needed, why := s.Check(ctx)
		if needed && s.planned != nil {
			s.planned()
		}
		res = append(res, PlanResult{ID: s.ID, Needed: needed, Why: why})
	}
	return res
}

// PrintPlan writes "+ id  why" for a needed step and "= id  why" for the
// others, and returns how many are needed.
func PrintPlan(w io.Writer, res []PlanResult) int {
	n := 0
	for _, r := range res {
		mark := "="
		if r.Needed {
			mark = "+"
			n++
		}
		fmt.Fprintf(w, "%s %-20s %s\n", mark, r.ID, r.Why)
	}
	return n
}

// UI is the terminal side of an install: one line per group of steps.
type UI struct {
	Out io.Writer
	TTY bool

	mu     sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	label  string
	n, tot int
	t0     time.Time
}

const (
	ansiReset = "\033[0m"
	ansiDim   = "\033[2m"
	ansiBold  = "\033[1m"
	ansiCyan1 = "\033[38;5;45m"
	ansiCyan2 = "\033[38;5;38m"
	ansiCyan3 = "\033[38;5;31m"
	ansiGreen = "\033[38;5;42m"
	ansiRed   = "\033[38;5;203m"
)

func (u *UI) c(code string) string {
	if u.TTY {
		return code
	}
	return ""
}

// Banner prints the header.
func (u *UI) Banner() {
	if !u.TTY {
		fmt.Fprint(u.Out, "CloudGround installer\n\n")
		return
	}
	fmt.Fprintf(u.Out, "\n      %s▄▆█▆▄%s\n", ansiCyan1, ansiReset)
	fmt.Fprintf(u.Out, "    %s▀▀▀▀▀▀▀▀▀%s   %sCloud%s%sGround%s\n", ansiCyan2, ansiReset, ansiBold, ansiReset, ansiDim, ansiReset)
	fmt.Fprintf(u.Out, "   %s━━━━━━━━━━━%s  %sperformance-first hosting control panel%s\n\n", ansiCyan3, ansiReset, ansiDim, ansiReset)
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (u *UI) begin(n, tot int, label string) {
	u.mu.Lock()
	u.n, u.tot, u.label, u.t0 = n, tot, label, time.Now()
	u.mu.Unlock()
	if !u.TTY {
		fmt.Fprintf(u.Out, "  [%d/%d] %s\n", n, tot, label)
		return
	}
	u.stop, u.done = make(chan struct{}), make(chan struct{})
	go func(stop, done chan struct{}) {
		defer close(done)
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i = (i + 1) % len(frames) {
			u.mu.Lock()
			fmt.Fprintf(u.Out, "\r  %s%s%s  %s%2d/%d%s  %s", ansiCyan1, frames[i], ansiReset, ansiDim, n, tot, ansiReset, label)
			u.mu.Unlock()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}(u.stop, u.done)
}

func (u *UI) spinnerStop() {
	if u.stop != nil {
		close(u.stop)
		<-u.done
		u.stop = nil
		fmt.Fprint(u.Out, "\r\033[2K")
	}
}

func (u *UI) end() {
	u.spinnerStop()
	secs := int(time.Since(u.t0).Seconds())
	fmt.Fprintf(u.Out, "  %s✓%s  %s%2d/%d%s  %-42s %s%ds%s\n",
		u.c(ansiGreen), u.c(ansiReset), u.c(ansiDim), u.n, u.tot, u.c(ansiReset), u.label, u.c(ansiDim), secs, u.c(ansiReset))
}

// Note prints a line under the current group.
func (u *UI) Note(msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.TTY {
		fmt.Fprint(u.Out, "\r\033[2K")
	}
	fmt.Fprintf(u.Out, "  %s·%s  %s\n", u.c(ansiDim), u.c(ansiReset), msg)
}

// Fail prints why the install stopped and the tail of the log.
func (u *UI) Fail(err error, logPath, logTail string) {
	u.spinnerStop()
	fmt.Fprintf(u.Out, "\n  %s✗  %v%s\n", u.c(ansiRed), err, u.c(ansiReset))
	if logPath != "" {
		fmt.Fprintf(u.Out, "     %sfull log: %s%s\n\n", u.c(ansiDim), logPath, u.c(ansiReset))
	}
	if logTail != "" {
		fmt.Fprint(u.Out, u.c(ansiDim))
		for _, l := range strings.Split(strings.TrimRight(logTail, "\n"), "\n") {
			fmt.Fprintf(u.Out, "     %s\n", l)
		}
		fmt.Fprint(u.Out, u.c(ansiReset)+"\n")
	}
}

// Apply runs the steps in order: each one's check, and its apply when the
// check says it is needed. The first failure stops the run.
func Apply(ctx context.Context, e *Env, steps []Step, u *UI) error {
	var groups []string
	for _, s := range steps {
		if len(groups) == 0 || groups[len(groups)-1] != s.Group {
			groups = append(groups, s.Group)
		}
	}
	gi := -1
	cur := ""
	for _, s := range steps {
		if s.Group != cur {
			if gi >= 0 && u != nil {
				u.end()
			}
			gi++
			cur = s.Group
			if u != nil {
				u.begin(gi+1, len(groups), cur)
			}
		}
		needed, why := s.Check(ctx)
		if !needed {
			e.logf("= %s: %s", s.ID, why)
			continue
		}
		e.logf("+ %s: %s", s.ID, why)
		if s.Apply == nil {
			return fmt.Errorf("%s: %s", s.ID, why)
		}
		if err := s.Apply(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
		if s.planned != nil {
			s.planned() // the later checks see what changed
		}
	}
	if gi >= 0 && u != nil {
		u.end()
	}
	return nil
}

var setupURL = regexp.MustCompile(`"url":"([^"]*)"`)

// SetupURL is SPEC §3 step 30: the one-time setup link the API logged at
// start, with the server's public IPv4 in it.
func SetupURL(ctx context.Context, e *Env) string {
	ip := publicIP(ctx, e)
	e.sleep(2 * time.Second)
	// The link carries the one-time setup token: read quietly, never logged.
	out, _ := e.Cmd.Quiet(ctx, "journalctl", "-u", "cloudground-api", "--since", "1 min ago", "-o", "cat")
	var url string
	for _, m := range setupURL.FindAllStringSubmatch(out, -1) {
		url = m[1]
	}
	if url == "" {
		return "https://" + ip + ":8443"
	}
	return strings.ReplaceAll(url, "<server-ip>", ip)
}

func publicIP(ctx context.Context, e *Env) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if b, err := e.Net.Fetch(ctx, "https://api.ipify.org"); err == nil {
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil {
			return ip.String()
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return "<server-ip>"
}

// IsSetupLink reports whether url is the one-time setup link (a first
// install) rather than the panel's address (an upgrade of a set-up panel).
func IsSetupLink(url string) bool { return strings.Contains(url, "/setup/") }

// Finished prints the end of a successful install. fwRules, from
// FirewallOffRules, adds the note that ufw is off and how to turn it on
// (SPEC §3 step 31); nil prints nothing about the firewall.
func (u *UI) Finished(url, logPath string, took time.Duration, fwRules []string) {
	d := u.c(ansiDim)
	r := u.c(ansiReset)
	fmt.Fprintf(u.Out, "\n  %s━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━%s\n\n", d, r)
	fmt.Fprintf(u.Out, "  %s✓%s  %sCloudGround is running%s   %sin %dm %ds%s\n\n",
		u.c(ansiGreen), r, u.c(ansiBold), r, d, int(took.Minutes()), int(took.Seconds())%60, r)
	fmt.Fprintf(u.Out, "     Open   %s%s%s\n\n", u.c(ansiCyan1), url, r)
	if IsSetupLink(url) {
		fmt.Fprintf(u.Out, "     %sThe link is valid for 24 hours and works once.%s\n", d, r)
		fmt.Fprintf(u.Out, "     %sYour browser will warn about the certificate — it is self-signed%s\n", d, r)
		fmt.Fprintf(u.Out, "     %suntil you issue a real one. That is expected on first boot.%s\n\n", d, r)
	} else {
		fmt.Fprintf(u.Out, "     %sThe panel was already set up: sign in as before.%s\n\n", d, r)
	}
	if len(fwRules) > 0 {
		cmds := make([]string, 0, len(fwRules)+1)
		for _, rule := range fwRules {
			cmds = append(cmds, "ufw allow "+rule)
		}
		cmds = append(cmds, "ufw enable")
		fmt.Fprintf(u.Out, "     %sThe firewall (ufw) is off. To turn it on with SSH, the sites and the panel open:%s\n", d, r)
		fmt.Fprintf(u.Out, "     %s%s%s\n\n", d, strings.Join(cmds, " && "), r)
	}
	fmt.Fprintf(u.Out, "     %slog %s%s\n\n", d, logPath, r)
}
