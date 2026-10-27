package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// #84 (SPEC §3.19): the list and the download are for the accounts that
// manage the site, a name that is not a log never reaches the agent, a log is
// streamed a chunk at a time with the domain of the route's site, a rotated
// one has its length and the current one has none, and each download is
// audited.
func TestSiteLogDownload(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "logs.example.com", state.SiteWordPress, "8.4", 0)
	other := seedActiveSite(t, s, "other.example.com", state.SiteStatic, "", 0)
	viewer := panelClient(t, s, ts.URL, "viewer@example.com", "readonly")
	base := fmt.Sprintf("/api/sites/%d/logs", site.ID)

	file := bytes.Repeat([]byte("0123456789"), (downloadChunk/10)+7)
	var mu sync.Mutex
	var offsets []int64
	var names, domains []string
	agent.setHook(func(method string) {
		if method != agentrpc.ReadSiteLogChunk.Name() {
			return
		}
		p := agent.last(method).(rpc.SiteLogChunkParams)
		mu.Lock()
		offsets = append(offsets, p.Offset)
		names = append(names, p.Name)
		domains = append(domains, p.Site.Domain)
		mu.Unlock()
		end := min(p.Offset+int64(p.Length), int64(len(file)))
		var data []byte
		if p.Offset < end {
			data = file[p.Offset:end]
		}
		agent.mu.Lock()
		agent.results[method] = rpc.SiteLogChunkResult{Data: data, Size: int64(len(file))}
		agent.mu.Unlock()
	})
	agent.mu.Lock()
	agent.results = map[string]any{agentrpc.ListSiteLogs.Name(): rpc.SiteLogsResult{Files: []rpc.SiteLogFile{{Name: "access.log", Size: 9}, {Name: "access.log.1", Size: 5}}}}
	agent.mu.Unlock()

	// Not a read-only account's, nor an anonymous caller's.
	for _, c := range []*client{viewer, {t: t, base: ts.URL}} {
		for _, p := range []string{base + "/files", base + "/download?file=access.log"} {
			if resp, _ := c.do("GET", p, nil); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s answered %d", p, resp.StatusCode)
			}
		}
	}
	if agent.called(agentrpc.ReadSiteLogChunk.Name()) || agent.called(agentrpc.ListSiteLogs.Name()) {
		t.Fatal("a refused caller reached the agent")
	}

	resp, body := admin.do("GET", base+"/files", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"access.log.1"`) {
		t.Fatalf("files = %d %s", resp.StatusCode, body)
	}

	// A name that is not one of the logs never reaches the agent.
	for _, name := range []string{"", "access", "../access.log", "..%2Faccess.log", "access.log%2F..%2F..%2Fetc%2Fpasswd", "/etc/passwd", "php-error.log", "access.log.bak", "access.log.1.zip", "other.example.com%2Faccess.log", "access.log%00.gz", "error.log.1234"} {
		resp, _ := admin.do("GET", base+"/download?file="+name, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("file=%q answered %d", name, resp.StatusCode)
		}
	}
	if agent.called(agentrpc.ReadSiteLogChunk.Name()) {
		t.Fatal("a name that is not a log reached the agent")
	}

	// A rotated log: streamed in chunks, with its length.
	resp, got := admin.do("GET", base+"/download?file=access.log.2.gz", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, file) {
		t.Fatalf("download = %d, %d bytes of %d", resp.StatusCode, len(got), len(file))
	}
	h := resp.Header
	if h.Get("Content-Length") != fmt.Sprint(len(file)) || h.Get("Content-Type") != "application/gzip" || h.Get("Cache-Control") != "no-store" ||
		h.Get("Content-Disposition") != `attachment; filename="logs.example.com-access.log.2.gz"` || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", h)
	}
	mu.Lock()
	read, asked, whose := fmt.Sprint(offsets), strings.Join(names, ","), strings.Join(domains, ",")
	mu.Unlock()
	if read != fmt.Sprintf("[0 %d]", downloadChunk) || asked != "access.log.2.gz,access.log.2.gz" || whose != "logs.example.com,logs.example.com" {
		t.Fatalf("chunks read at %s of %s for %s", read, asked, whose)
	}

	// The current log: no length, read until the agent has no more.
	mu.Lock()
	offsets, names, domains = nil, nil, nil
	mu.Unlock()
	resp, got = admin.do("GET", base+"/download?file=error.log", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, file) || resp.Header.Get("Content-Length") != "" || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("the current log = %d, %d bytes, headers %v", resp.StatusCode, len(got), resp.Header)
	}
	mu.Lock()
	read = fmt.Sprint(offsets)
	mu.Unlock()
	if read != fmt.Sprintf("[0 %d %d]", downloadChunk, len(file)) {
		t.Fatalf("the current log's chunks at %s", read)
	}

	// A log that is not there is 404; the site is always the route's, never the query's.
	agent.setHook(nil)
	agent.mu.Lock()
	agent.errs[agentrpc.ReadSiteLogChunk.Name()] = agentrpc.NotFound("no such log")
	agent.mu.Unlock()
	if resp, _ := admin.do("GET", base+"/download?file=access.log.7", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a log that is not there answered %d", resp.StatusCode)
	}
	agent.mu.Lock()
	delete(agent.errs, agentrpc.ReadSiteLogChunk.Name())
	agent.mu.Unlock()
	if resp, _ := admin.do("GET", base+"/download?file=access.log&site=other.example.com&domain=other.example.com", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	if p := agent.last(agentrpc.ReadSiteLogChunk.Name()).(rpc.SiteLogChunkParams); p.Site.ID != site.ID || p.Site.ID == other.ID {
		t.Fatalf("the agent was asked about site %d", p.Site.ID)
	}

	// Audited: who, which site, which file.
	resp, audit := admin.do("GET", "/api/audit", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(audit), `"site.logs.download"`) || !strings.Contains(string(audit), "access.log.2.gz") {
		t.Fatalf("audit = %d %s", resp.StatusCode, head(audit, 600))
	}
}

func head(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
