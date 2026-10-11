//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

type serviceRow struct {
	Name        string    `json:"name"`
	Unit        string    `json:"unit"`
	State       string    `json:"state"`
	Active      bool      `json:"active"`
	Detail      string    `json:"detail"`
	MemoryBytes int64     `json:"memory_bytes"`
	Since       time.Time `json:"since"`
	Version     string    `json:"version"`
}

// inGroup reports whether the account database lists user in group.
func (h *harness) inGroup(user, group string) bool {
	for _, g := range strings.Fields(h.run("id", "-nG", user).Out) {
		if g == group {
			return true
		}
	}
	return false
}

// shadowVerifies is "ok" when user's shadow hash is the hash of password,
// checked with libcrypt through perl (present on every Ubuntu).
func (h *harness) shadowVerifies(user, password string) string {
	b, err := os.ReadFile("/etc/shadow")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 1 && f[0] == user {
			return h.run("perl", "-e", `print crypt($ARGV[0], $ARGV[1]) eq $ARGV[1] ? "ok" : "mismatch"`, password, f[1]).Trim()
		}
	}
	return "no shadow entry"
}

func (h *harness) serviceRows() (int, map[string]serviceRow) {
	c, b := h.api.get("/api/services")
	var rows []serviceRow
	_ = json.Unmarshal(b, &rows)
	m := map[string]serviceRow{}
	for _, r := range rows {
		m[r.Name] = r
	}
	return c, m
}

// services (R-2.4): the services page reads systemd over D-Bus, and a restart
// returns only once systemd's job has finished. Each value is compared with
// what systemctl itself reports on the box, not with the API's own answer.
func (h *harness) services(t *testing.T) {
	c, rows := h.serviceRows()
	h.check(t, "R-2.4: the services page answers", strconv.Itoa(c), "200")
	for _, name := range []string{"nginx", "mariadb"} {
		r := rows[name]
		unit := map[string]string{"nginx": "nginx.service", "mariadb": "mariadb.service"}[name]
		h.check(t, "R-2.4: "+name+" state matches systemd",
			r.Detail, h.run("systemctl", "show", "-p", "ActiveState", "--value", unit).Trim())
		h.check(t, "R-2.4: "+name+" reads as running", r.State, "running")
		h.assert(t, "R-2.4: "+name+" reports its cgroup memory", r.MemoryBytes > 0, "%s memory_bytes = %d", name, r.MemoryBytes)
		h.assert(t, "R-2.4: "+name+" reports when it started",
			!r.Since.IsZero() && r.Since.Before(time.Now().Add(time.Minute)) && r.Since.After(time.Now().AddDate(-1, 0, 0)),
			"%s since = %v", name, r.Since)
		h.assert(t, "R-2.4: "+name+" reports its version", r.Version != "", "%s has no version", name)
	}
	// Redis is no longer installed (R-6.1); an upgraded server keeps it,
	// switched off.
	redisUnit := func() string {
		return h.run("systemctl", "show", "-p", "LoadState", "--value", "redis-server.service").Trim()
	}
	redisInstalled := redisUnit() != "not-found"
	if redisInstalled {
		redis := h.run("systemctl", "show", "-p", "ActiveState", "--value", "redis-server.service").Trim()
		h.check(t, "R-2.4: redis state matches systemd", rows["redis"].Detail, redis)
	} else {
		r, listed := rows["redis"]
		h.assert(t, "R-2.4: a Redis that is not installed has no row", !listed, "redis row %+v", r)
	}
	_, hasPHP := rows["site-php"]
	h.assert(t, "R-2.4: site PHP masters are summarised", hasPHP, "no site-php row")

	before := h.mainPID("nginx.service")
	h.check(t, "R-2.4: nginx restart is accepted", code(h.api.post("/api/services/nginx/restart", `{}`)), "200")
	// No wait here: the call returns after the restart job completed.
	after := h.mainPID("nginx.service")
	h.assert(t, "R-2.4: the restart replaced nginx's main process", after != "" && after != "0" && after != before,
		"MainPID %s -> %s", before, after)
	h.check(t, "R-2.4: nginx is active when the restart call returns",
		h.run("systemctl", "is-active", "nginx.service").Trim(), "active")
	h.check(t, "R-2.4: an unmanaged service is refused", code(h.api.post("/api/services/ssh/restart", `{}`)), "400")

	// Where Redis is installed it is off (the installer switches it off). A restart of a stopped
	// unit starts it, and the API took one although the page shows no
	// button: Redis then listened on 127.0.0.1:6379 for every site's PHP
	// and shell.
	redisOff := func() string {
		return h.run("systemctl", "show", "-p", "ActiveState", "--value", "redis-server.service").Trim() + "/" +
			h.run("systemctl", "show", "-p", "UnitFileState", "--value", "redis-server.service").Trim()
	}
	if !redisInstalled {
		// A restart of a service that is not installed is a refusal that
		// says so, not the failed job's 502.
		c, b := h.api.post("/api/services/redis/restart", `{}`)
		h.assert(t, "R-2.4: a restart of a Redis that is not installed is refused", c == 409 && contains(b, "not installed"),
			"restart answered %d: %.300s", c, b)
		h.check(t, "R-2.4: the refused restart left no Redis unit", redisUnit(), "not-found")
		return
	}
	h.check(t, "R-2.4 control: Redis is stopped and disabled", redisOff(), "inactive/disabled")
	h.check(t, "R-2.4: a restart of the disabled Redis is refused", code(h.api.post("/api/services/redis/restart", `{}`)), "409")
	h.check(t, "R-2.4: the refused restart left Redis off", redisOff(), "inactive/disabled")
}
