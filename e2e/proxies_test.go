//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// R-4.3: the proxies and the system routes rewritten from their spec
// (internal/api/SPEC.md §6-§13). What a box shows of it: the connector's
// purge limits wrong tokens and never the right one, and alerts are rows
// of a table the dashboard reads without asking the agent.

// limitedFrom is the address the connector checks send from: a loopback
// address of their own, so the limit they trip is not the one the sites
// (127.0.0.1) purge under, and a later section's purges are not refused.
const limitedFrom = "127.0.0.2"

var limitedClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
	DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(limitedFrom)}}).DialContext,
}}

// connectorPurge posts one purge to the loopback listener, as a site's
// connector does, from limitedFrom, and returns the status.
func connectorPurge(token, body string) int {
	req, err := http.NewRequest("POST", "http://127.0.0.1:9080/api/connector/purge", strings.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := limitedClient.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) proxiesAlerts(t *testing.T) {
	f, ok := h.featureSite(t)
	if !ok {
		return
	}
	h.connectorLimit(t, f)
	h.alertsTable(t, f)
}

// connectorLimit: 25 wrong tokens from one address, then the feature
// site's own token from the same address. Before R-4.3 a limited address
// was refused before its token was looked at, so the valid purge got 429
// too; and since every site's PHP purges from 127.0.0.1, one site could
// stop every other site's purges for a minute.
func (h *harness) connectorLimit(t *testing.T, f *feat) {
	tok := strings.TrimSpace(h.wp(f.fsu, f.fp, "config", "get", "CLOUDGROUND_SITE_TOKEN").Out)
	h.assert(t, "R-4.3 control: the feature site has a connector token", len(tok) >= 16, "no connector token in wp-config.php")
	if len(tok) < 16 {
		return
	}
	purge := fmt.Sprintf(`{"urls":["https://%s/cg-e2e-r43/"]}`, f.fs)
	h.check(t, "R-4.3 control: the site's own token purges", strconv.Itoa(connectorPurge(tok, purge)), "200")
	first, last := 0, 0
	for i := 0; i < 25; i++ {
		c := connectorPurge(fmt.Sprintf("cg-e2e-wrong-%d-%d", h.pid, i), purge)
		if i == 0 {
			first = c
		}
		last = c
	}
	h.check(t, "R-4.3: a wrong connector token answers 401", strconv.Itoa(first), "401")
	h.check(t, "R-4.3: wrong tokens past the limit answer 429", strconv.Itoa(last), "429")
	h.check(t, "R-4.3: the site's own token still purges from the limited address",
		strconv.Itoa(connectorPurge(tok, purge)), "200")
}

type e2eAlert struct {
	ID, Kind, Message, Level string
	SiteID                   int64 `json:"site_id"`
}

func (h *harness) alerts() []e2eAlert {
	var list []e2eAlert
	_, b := h.api.get("/api/system/alerts")
	_ = json.Unmarshal(b, &list)
	return list
}

// alertsTable: a task that fails is an alert within seconds (the watcher
// evaluates after task.finished), a dismissal survives a re-evaluation, and
// reading the list asks the agent nothing, so it is a fast database read
// (before R-4.3 every GET called ServiceStatus on the agent).
func (h *harness) alertsTable(t *testing.T, f *feat) {
	// A task that fails: the import of a file the site does not have. (It
	// was an update of an unknown target, which #20 refuses with a 400
	// before any task: it would cost a snapshot first.) Its alert is the
	// task alert of the site that was not there before.
	before := map[string]bool{}
	for _, a := range h.alerts() {
		before[a.ID] = true
	}
	missing := fmt.Sprintf("shared/cg-e2e-nothing-%d.sql", h.pid)
	_, b := h.api.post(f.api("/database/import"), fmt.Sprintf(`{"path":%q,"confirm":%q}`, missing, f.fs))
	h.check(t, "R-4.3 control: an import of nothing fails", h.api.waitTask(taskID(b), 120*time.Second), "failed")
	var found e2eAlert
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && found.ID == ""; time.Sleep(500 * time.Millisecond) {
		for _, a := range h.alerts() {
			if a.Kind == "task" && a.SiteID == f.fid && !before[a.ID] {
				found = a
			}
		}
	}
	h.assert(t, "R-4.3: the failed task is an alert of its site", found.ID != "", "no task alert for the failed update")
	if found.ID == "" {
		return
	}
	h.check(t, "R-4.3: the alert can be dismissed", code(h.api.post("/api/system/alerts/"+found.ID+"/dismiss", `{}`)), "200")
	c, _ := h.api.runMaintenance()
	h.check(t, "R-4.3 control: a maintenance tick re-evaluates the alerts", strconv.Itoa(c), "200")
	still := false
	for _, a := range h.alerts() {
		still = still || a.ID == found.ID
	}
	h.assert(t, "R-4.3: a dismissed alert stays dismissed after a re-evaluation", !still, "alert %s came back", found.ID)

	var took []time.Duration
	for i := 0; i < 7; i++ {
		start := time.Now()
		cd, _ := h.api.get("/api/system/alerts")
		took = append(took, time.Since(start))
		if cd != 200 {
			took[i] = time.Hour
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	h.assert(t, "R-4.3: the alert list is a database read (median of 7 under 60 ms)", took[3] < 60*time.Millisecond,
		"median GET /api/system/alerts took %v", took[3])
}
