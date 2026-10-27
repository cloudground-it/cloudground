//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/api"
)

// routeMatrix (R-4.1): the live panel, through its socket, refuses every
// route of the API's route table as the table says, for a caller with no
// session and for an operator assigned only site own. Only refusals are
// probed: a refused request never reaches its handler, so nothing on the box
// changes unless the router lets through what it should not.
//
// cron is a cron job of site other, whose 404 shows a job is checked against
// its site; the operator's own site log is the control that its session
// works.
func (h *harness) routeMatrix(t *testing.T, op *apiClient, own, other int64, cron string) {
	anon := newAPIClient(h, "", "")
	h.check(t, "R-4.1 control: the operator's session works", code(op.get(fmt.Sprintf("/api/sites/%d/logs?source=access", own))), "200")

	var anonWrong, opWrong []string
	probes := 0
	for _, rt := range api.RouteSummaries() {
		// The MCP server's rows judge their own callers and answer 404
		// while it is off (api SPEC §15); section "mcp server" covers them.
		if rt.Method == "" || rt.Role == "open" || rt.Role == "site token" || rt.Role == "mcp" {
			continue
		}
		id := "999999" // no such backup or release: 404 whoever asks
		switch rt.Resource {
		case "site":
			id = fmt.Sprint(other)
		case "cron job":
			id = cron
		}
		path := strings.NewReplacer("{id}", id, "{name}", "nginx", "{fingerprint}", "SHA256:none",
			"{table}", "wp_options", "{dir}", "exports", "{db}", "site_1").Replace(rt.Path)
		var body any
		if rt.Method != http.MethodGet {
			body = "{}"
		}
		probes++

		// Quarry's routes take its own session (F9b): a panel session
		// is no more one than none at all.
		studio := strings.HasPrefix(rt.Role, "dbstudio")
		anonMsg := "authentication required"
		if studio {
			anonMsg = "sign in to Quarry"
		}
		if status, msg := refusalOf(anon.do(rt.Method, path, body)); status != 401 || msg != anonMsg {
			anonWrong = append(anonWrong, fmt.Sprintf("%s %s: %d %q", rt.Method, rt.Path, status, msg))
		}

		want, wantMsg := 0, ""
		switch {
		case studio:
			want, wantMsg = 401, "sign in to Quarry"
		case rt.Role == "global":
			want, wantMsg = 403, "server-wide access required"
		case rt.Role == "admin":
			// W3: an operator is told it needs an administrator ("only
			// administrators can create sites"), not that its account is
			// read-only, which it is not.
			want, wantMsg = 403, rt.OperatorRefusal
		case rt.Resource != "":
			want, wantMsg = 404, rt.Resource+" not found"
		}
		if want == 0 {
			continue // an operator may call it: not probed
		}
		if status, msg := refusalOf(op.do(rt.Method, path, body)); status != want || msg != wantMsg {
			opWrong = append(opWrong, fmt.Sprintf("%s %s: %d %q, want %d %q", rt.Method, rt.Path, status, msg, want, wantMsg))
		}
	}
	_, msg := refusalOf(op.do("POST", "/api/sites", `{}`))
	h.check(t, "W3: an operator creating a site is told only administrators can", msg, "only administrators can create sites")
	h.assert(t, "R-4.1: the route table has routes to probe", probes > 50, "only %d routes in the table", probes)
	h.assert(t, "R-4.1: every non-public route refuses a request without a session with 401", len(anonWrong) == 0,
		"%d routes did not: %s", len(anonWrong), strings.Join(anonWrong, "; "))
	h.assert(t, "R-4.1: an operator is refused every server-wide and admin route (403) and another site's resources (404)", len(opWrong) == 0,
		"%d routes did not: %s", len(opWrong), strings.Join(opWrong, "; "))
}

// refusalOf reads a response's status and its {"error"} message.
func refusalOf(status int, body []byte) (int, string) {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return status, e.Error
}
