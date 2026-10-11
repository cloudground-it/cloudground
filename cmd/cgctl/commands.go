package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// request is one API call a command makes. siteSummary: its answer is a
// site creation's, printed as the summary (sitecreate.go) instead of JSON.
// warnings go to stderr before the call is made.
type request struct {
	method, path string
	body         any
	siteSummary  bool
	warnings     []string
}

// command maps words on the command line to an API call.
type command struct {
	words []string // e.g. "sites", "create"
	args  string   // usage of the arguments
	min   int      // fewest arguments
	max   int      // most arguments; -1 = any
	build func(a []string) (request, error)
	// password: the call must be made signed in with a password (the API
	// refuses it with a token).
	password bool
}

func get(path string) func([]string) (request, error) {
	return func([]string) (request, error) { return request{method: "GET", path: path, body: nil}, nil }
}

// id checks a numeric ID argument, so nothing else lands in a URL path.
func id(s string) (string, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%q is not an id", s)
	}
	return strconv.FormatInt(n, 10), nil
}

// call builds a request whose path has the first argument as an id.
func call(method, format string, body func(a []string) (any, error)) func([]string) (request, error) {
	return func(a []string) (request, error) {
		n, err := id(a[0])
		if err != nil {
			return request{}, err
		}
		var b any = map[string]any{}
		if body != nil {
			if b, err = body(a); err != nil {
				return request{}, err
			}
		}
		return request{method: method, path: fmt.Sprintf(format, n), body: b}, nil
	}
}

var commands = []command{
	{words: []string{"sites", "list"}, build: get("/api/sites")},
	{words: []string{"sites", "create"}, args: siteCreateArgs, min: 2, max: -1, build: buildSiteCreate},
	{words: []string{"sites", "delete"}, args: "<site-id>", min: 1, max: 1, build: call("DELETE", "/api/sites/%s", nil)},
	{words: []string{"purge"}, args: "<site-id> [url ...]", min: 1, max: -1, build: call("POST", "/api/sites/%s/purge", func(a []string) (any, error) {
		if len(a) > 1 {
			return map[string]any{"urls": a[1:]}, nil
		}
		return map[string]any{}, nil
	})},
	{words: []string{"warm"}, args: "<site-id>", min: 1, max: 1, build: call("POST", "/api/sites/%s/warm", nil)},
	{words: []string{"staging", "create"}, args: "<site-id>", min: 1, max: 1, build: call("POST", "/api/sites/%s/staging", nil)},
	{words: []string{"deploy"}, args: "<site-id> <source-dir>", min: 2, max: 2, build: call("POST", "/api/sites/%s/deployments", func(a []string) (any, error) {
		return map[string]string{"source_dir": a[1]}, nil
	})},
	{words: []string{"safe-push"}, args: "<site-id> [path ...]", min: 1, max: -1, build: call("POST", "/api/sites/%s/safe-push", func(a []string) (any, error) {
		if len(a) > 1 {
			return map[string]any{"paths": a[1:]}, nil
		}
		return map[string]any{}, nil
	})},
	{words: []string{"rollback"}, args: "<site-id>", min: 1, max: 1, build: call("POST", "/api/sites/%s/rollback", nil)},
	{words: []string{"backup", "run"}, args: "<site-id>", min: 1, max: 1, build: call("POST", "/api/sites/%s/backups", nil)},
	{words: []string{"backup", "verify"}, args: "<backup-id>", min: 1, max: 1, build: call("POST", "/api/backups/%s/verify", nil)},
	{words: []string{"backup", "restore"}, args: "<backup-id> <domain> [full|files|database]", min: 2, max: 3, build: call("POST", "/api/backups/%s/restore", func(a []string) (any, error) {
		mode := "full"
		if len(a) == 3 {
			mode = a[2]
		}
		return map[string]string{"confirm": a[1], "mode": mode}, nil
	})},
	{words: []string{"database", "import"}, args: "<site-id> <domain> <site-relative.sql> [database]", min: 3, max: 4, build: call("POST", "/api/sites/%s/database/import", func(a []string) (any, error) {
		body := map[string]string{"confirm": a[1], "path": a[2]}
		if len(a) == 4 {
			body["database"] = a[3]
		}
		return body, nil
	})},
	{words: []string{"database", "list"}, args: "<site-id>", min: 1, max: 1, build: call("GET", "/api/sites/%s/databases", nil)},
	{words: []string{"database", "create"}, args: "<site-id> <name>", min: 2, max: 2, build: call("POST", "/api/sites/%s/databases", func(a []string) (any, error) {
		return map[string]string{"name": a[1]}, nil
	})},
	{words: []string{"database", "user-create"}, args: "<site-id> <name> [database:all|read ...]", min: 2, max: -1, build: call("POST", "/api/sites/%s/database-users", func(a []string) (any, error) {
		grants := []map[string]string{}
		for _, g := range a[2:] {
			db, level, ok := strings.Cut(g, ":")
			if !ok || (level != "all" && level != "read") {
				return nil, fmt.Errorf("a grant is database:all or database:read, not %q", g)
			}
			grants = append(grants, map[string]string{"database": db, "level": level})
		}
		return map[string]any{"name": a[1], "grants": grants}, nil
	})},
	{words: []string{"cron", "run"}, args: "<cron-id>", min: 1, max: 1, build: call("POST", "/api/cron/%s/run", nil)},
	{words: []string{"cert", "issue"}, args: "<site-id>", min: 1, max: 1, build: call("POST", "/api/sites/%s/certificate", nil)},
	{words: []string{"status"}, build: get("/api/system/status")},
	{words: []string{"drift"}, build: get("/api/system/drift")},
	{words: []string{"tasks"}, build: get("/api/tasks")},
	{words: []string{"task"}, args: "<task-id>", min: 1, max: 1, build: call("GET", "/api/tasks/%s", nil)},
	{words: []string{"audit"}, build: get("/api/audit")},
	{words: []string{"settings", "get"}, build: get("/api/settings")},
	{words: []string{"settings", "set"}, args: "<key> <value>", min: 2, max: 2, build: func(a []string) (request, error) {
		return request{method: "PUT", path: "/api/settings", body: map[string]string{a[0]: a[1]}}, nil
	}},
	{words: []string{"tokens", "list"}, password: true, build: get("/api/tokens")},
	{words: []string{"tokens", "create"}, args: "<name> [expires-in-days]", min: 1, max: 2, password: true, build: func(a []string) (request, error) {
		body := map[string]any{"name": a[0]}
		if len(a) == 2 {
			days, err := strconv.Atoi(a[1])
			if err != nil {
				return request{}, fmt.Errorf("expires-in-days: %q is not a number", a[1])
			}
			body["expires_in_days"] = days
		}
		return request{method: "POST", path: "/api/tokens", body: body}, nil
	}},
	{words: []string{"tokens", "revoke"}, args: "<token-id>", min: 1, max: 1, password: true, build: call("DELETE", "/api/tokens/%s", nil)},
}

func commandList() string {
	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  cgctl %s %s\n", strings.Join(c.words, " "), c.args)
	}
	return b.String()
}

// invocation is a parsed command line.
type invocation struct {
	cmd     command
	req     request
	wait    bool
	timeout time.Duration
}

var errHelp = errors.New("help")

// parseArgs takes the global flags from anywhere on the line, then matches
// the longest command whose words lead the rest.
func parseArgs(args []string) (invocation, error) {
	inv := invocation{timeout: time.Hour}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "help":
			return inv, errHelp
		case a == "--wait" || a == "-w":
			inv.wait = true
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, ok := strings.CutPrefix(a, "--timeout=")
			if !ok {
				if i+1 >= len(args) {
					return inv, errors.New("--timeout needs a duration such as 30m")
				}
				i++
				v = args[i]
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return inv, fmt.Errorf("--timeout: %q is not a duration such as 30m", v)
			}
			inv.timeout = d
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return inv, errHelp
	}
	best := -1
	for i, c := range commands {
		if len(rest) >= len(c.words) && slices.Equal(rest[:len(c.words)], c.words) && (best < 0 || len(c.words) > len(commands[best].words)) {
			best = i
		}
	}
	if best < 0 {
		return inv, fmt.Errorf("unknown command %q", strings.Join(rest, " "))
	}
	inv.cmd = commands[best]
	a := rest[len(inv.cmd.words):]
	if len(a) < inv.cmd.min || (inv.cmd.max >= 0 && len(a) > inv.cmd.max) {
		return inv, fmt.Errorf("usage: cgctl %s %s", strings.Join(inv.cmd.words, " "), inv.cmd.args)
	}
	req, err := inv.cmd.build(a)
	if err != nil {
		return inv, err
	}
	inv.req = req
	return inv, nil
}
