package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// An agent error reaches the browser as its message only: the detail, which
// names commands and host paths, stays out of the response.
func TestAgentErrorsReachTheBrowserWithoutDetail(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site, err := s.Store.CreateSite(state.Site{Domain: "err.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced,
		Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/err.example.com", Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/sites/%d/files?path=nope", site.ID)
	const detail = "lstat /srv/sites/err.example.com/nope: runuser -u cg-site-1 -- ls: exit status 2"
	for _, tc := range []struct {
		err     error
		status  int
		message string
	}{
		{&agentrpc.Error{Code: agentrpc.CodeInternal, Message: "Could not list the files", Detail: detail}, http.StatusBadGateway, "Could not list the files"},
		{&agentrpc.Error{Code: agentrpc.CodeInvalid, Message: "path escapes site root", Detail: detail}, http.StatusBadRequest, "path escapes site root"},
		{&agentrpc.Error{Code: agentrpc.CodeConflict, Message: "site err.example.com no longer exists", Detail: detail}, http.StatusConflict, "site err.example.com no longer exists"},
		{&agentrpc.Error{Code: agentrpc.CodeTimeout, Message: "Could not list the files: the agent's time limit ran out", Detail: detail}, http.StatusGatewayTimeout, "Could not list the files: the agent's time limit ran out"},
		{&agentrpc.Error{Code: agentrpc.CodeUnavailable, Message: "The agent is not reachable", Detail: "dial unix /run/cloudground/agent.sock: connect: no such file"}, http.StatusServiceUnavailable, "The agent is not reachable"},
		// An error that is not the agent's own type is never trusted as text.
		{errors.New(detail), http.StatusBadGateway, "The agent could not complete the operation"},
	} {
		agent.errs[agentrpc.ListFiles.Name()] = tc.err
		resp, body := c.do("GET", path, nil)
		if resp.StatusCode != tc.status {
			t.Errorf("%v: status %d, want %d", tc.err, resp.StatusCode, tc.status)
		}
		if !strings.Contains(string(body), tc.message) {
			t.Errorf("%v: body %s lacks %q", tc.err, body, tc.message)
		}
		for _, leak := range []string{"/srv/", "/run/", "runuser", "lstat", "exit status"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%v: body leaks %q: %s", tc.err, leak, body)
			}
		}
	}
}

// No handler hands an agent error's text to respondErr: they all go through
// respondAgentErr. A source check, so a new call site cannot slip back.
func TestNoAgentErrorTextInResponses(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	relay := regexp.MustCompile(`respondErr\([^\n]*err\.Error\(\)`)
	call := regexp.MustCompile(`agentrpc\.(Into|Call)\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !relay.MatchString(l) {
				continue
			}
			// Walk back to the statement that set err; a closing "})" means
			// err came from the task runner, not from an agent call inside it.
			for j := i - 1; j >= max(0, i-6); j-- {
				if strings.TrimSpace(lines[j]) == "})" {
					break
				}
				if call.MatchString(lines[j]) {
					t.Errorf("%s:%d relays an agent error's text: %s", f, i+1, strings.TrimSpace(l))
					break
				}
			}
		}
	}
}
