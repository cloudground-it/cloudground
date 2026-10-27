//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// siteLogDownload (#84): a static site's access log is downloaded in full
// through the API with the request that was just made in it; after the
// installed logrotate configuration has really rotated the site's logs
// (twice, so one is compressed) the rotated files are listed and downloaded
// with their exact lengths, and the compressed one is gzip with the older
// request in it; a name that is not a log, a path, or a link to another file
// is never served; the downloads are audited; cgctl streams a log to a file
// and does not overwrite one.
func (h *harness) siteLogDownload(t *testing.T) {
	dom := h.dom("logdl84")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#84: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	h.mustWriteAs(t, "/srv/sites/"+dom+"/current/index.html", "cg-84", h.api.site(id).SystemUser)
	dir := "/var/log/cloudground/" + dom
	base := fmt.Sprintf("/api/sites/%d/logs", id)
	markA, markB := fmt.Sprintf("/cg84-a-%d", h.pid), fmt.Sprintf("/cg84-b-%d", h.pid)

	// untilLogged waits for nginx's buffered log (a second at most) to hold mark.
	untilLogged := func(mark string) bool {
		for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
			if b, _ := os.ReadFile(dir + "/access.log"); strings.Contains(string(b), mark) {
				return true
			}
		}
		return false
	}
	download := func(name string) (int, http.Header, []byte) {
		return h.api.getRaw(base + "/download?file=" + name)
	}

	h.get(dom, markA)
	h.assert(t, "#84 control: nginx logs the request", untilLogged(markA), "%s not in %s/access.log", markA, dir)
	var list struct {
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	c, b := h.api.get(base + "/files")
	_ = json.Unmarshal(b, &list)
	names := func() string {
		var n []string
		for _, f := range list.Files {
			n = append(n, f.Name)
		}
		return strings.Join(n, " ")
	}
	h.assert(t, "#84: the list has the current access log, with its size", c == 200 && strings.Contains(names(), "access.log") && list.Files[0].Size > 0, "= %d %s", c, head(b, 300))

	code, hdr, body := download("access.log")
	h.assert(t, "#84: the current access log downloads whole, with the request just made in it", code == 200 && strings.Contains(string(body), markA), "= %d %s", code, head(body, 300))
	// The handler declares no length for the file nginx is writing; net/http
	// still sends the exact one of a body short enough to fit its buffer.
	cl := hdr.Get("Content-Length")
	h.assert(t, "#84: as an attachment named after the site, with no length up front", strings.Contains(hdr.Get("Content-Disposition"), `filename="`+dom+`-access.log"`) && (cl == "" || cl == fmt.Sprint(len(body))) && hdr.Get("Cache-Control") == "no-store",
		"%v (body %d bytes)", hdr, len(body))
	code, _, body = download("error.log")
	h.check(t, "#84: the error log downloads too", fmt.Sprint(code), "200")
	_ = body

	// Real rotation by the installed configuration, limited to this site.
	conf, err := os.ReadFile("/etc/logrotate.d/cloudground")
	if err != nil {
		h.fail(t, "#84 control: the installed logrotate configuration is readable", "%v", err)
		return
	}
	tmp := h.run("mktemp", "-d").Trim()
	defer h.run("rm", "-rf", tmp)
	cfg := strings.Replace(string(conf), "/var/log/cloudground/*/*.log", dir+"/*.log", 1)
	if cfg == string(conf) {
		h.fail(t, "#84 control: the configuration names the sites' logs", "%s", head(conf, 300))
		return
	}
	cfgPath := filepath.Join(tmp, "rotate.conf")
	_ = os.WriteFile(cfgPath, []byte(cfg), 0o644)
	rotate := func() result { return h.run("logrotate", "-f", "-s", filepath.Join(tmp, "state"), cfgPath) }
	h.assert(t, "#84: the site's logs are rotated by the installed configuration", rotate().OK(), "logrotate failed")
	h.get(dom, markB)
	h.assert(t, "#84 control: the next request is logged in the new file", untilLogged(markB), "%s not in %s/access.log", markB, dir)
	h.assert(t, "#84: and rotated again", rotate().OK(), "logrotate failed")

	_, b = h.api.get(base + "/files")
	list.Files = nil
	_ = json.Unmarshal(b, &list)
	h.assert(t, "#84: the list has the rotated logs, plain and compressed", strings.Contains(names(), "access.log.1") && strings.Contains(names(), "access.log.2.gz"), "%s", names())
	code, hdr, body = download("access.log.1")
	st, _ := os.Stat(dir + "/access.log.1")
	h.assert(t, "#84: the first rotated log is the later request, whole, with its exact length",
		code == 200 && strings.Contains(string(body), markB) && !strings.Contains(string(body), markA) && st != nil && hdr.Get("Content-Length") == fmt.Sprint(st.Size()), "= %d, length %q, %s", code, hdr.Get("Content-Length"), head(body, 200))
	code, hdr, body = download("access.log.2.gz")
	gz, gerr := gzip.NewReader(bytes.NewReader(body))
	var plain []byte
	if gerr == nil {
		plain, _ = io.ReadAll(gz)
	}
	st, _ = os.Stat(dir + "/access.log.2.gz")
	h.assert(t, "#84: the compressed log is gzip with the earlier request in it, with its exact length",
		code == 200 && gerr == nil && strings.Contains(string(plain), markA) && hdr.Get("Content-Type") == "application/gzip" && st != nil && hdr.Get("Content-Length") == fmt.Sprint(st.Size()),
		"= %d, gzip error %v, length %q", code, gerr, hdr.Get("Content-Length"))

	// What is never served.
	for _, name := range []string{"../../../../etc/passwd", "..%2F..%2F..%2Fetc%2Fpasswd", "%2Fetc%2Fpasswd", "access.log%2F..%2F..%2Ftmp", "php-error.log", "access.log.bak", ""} {
		code, _, body := download(name)
		h.assert(t, "#84: file="+name+" is refused", code == 400 && !strings.Contains(string(body), "root:"), "= %d %s", code, head(body, 200))
	}
	if err := os.Symlink("/etc/passwd", dir+"/access.log.7"); err == nil {
		code, _, body := download("access.log.7")
		h.assert(t, "#84: a link in the log directory to another file is not served", code != 200 && !strings.Contains(string(body), "root:"), "= %d %s", code, head(body, 200))
		_, b = h.api.get(base + "/files")
		h.assert(t, "#84: nor listed", !strings.Contains(string(b), "access.log.7"), "%s", head(b, 300))
	}
	code, _, _ = download("access.log.9")
	h.check(t, "#84: a rotation that is not there is 404", fmt.Sprint(code), "404")
	c, _ = h.api.get(fmt.Sprintf("/api/sites/%d/logs?source=access", id))
	h.check(t, "#84 control: the viewer's tail still works", fmt.Sprint(c), "200")

	_, b = h.api.get("/api/audit")
	h.assert(t, "#84: the downloads are audited", strings.Contains(string(b), "site.logs.download") && strings.Contains(string(b), "access.log.2.gz"), "%s", head(b, 400))

	// cgctl: a log of any size goes to a file as it arrives, never over one.
	name := fmt.Sprintf("e2e-%d-logs", h.pid)
	c, b = h.api.post("/api/tokens", map[string]any{"name": name, "expires_in_days": 1})
	var created struct {
		Token    string `json:"token"`
		APIToken struct {
			ID int64 `json:"id"`
		} `json:"api_token"`
	}
	_ = json.Unmarshal(b, &created)
	h.check(t, "#84: an API token for cgctl is created", fmt.Sprint(c), "201")
	if created.Token == "" {
		return
	}
	h.undo("revoke the e2e logs API token", func() { h.api.del(fmt.Sprintf("/api/tokens/%d", created.APIToken.ID)) })
	tokenFile := filepath.Join(tmp, "token")
	_ = os.WriteFile(tokenFile, []byte(created.Token+"\n"), 0o600)
	cgctl := func(args ...string) result {
		env := []string{"-u", "CLOUDGROUND_EMAIL", "-u", "CLOUDGROUND_PASSWORD", "-u", "CLOUDGROUND_TOKEN", "-u", "CLOUDGROUND_CONFIG",
			"CLOUDGROUND_TOKEN_FILE=" + tokenFile, cgctlBinary}
		return h.run("env", append(env, args...)...)
	}
	out := filepath.Join(tmp, "access.log.2.gz")
	r := cgctl("logs", fmt.Sprint(id), "access.log.2.gz", out)
	disk, _ := os.ReadFile(out)
	same, _ := os.ReadFile(dir + "/access.log.2.gz")
	h.assert(t, "#84: cgctl writes the log to the file asked for, byte for byte", r.OK() && len(disk) > 0 && bytes.Equal(disk, same), "%s (%d bytes, on the server %d)", r, len(disk), len(same))
	r = cgctl("logs", fmt.Sprint(id), "access.log.2.gz", out)
	h.assert(t, "#84: and does not overwrite a file", !r.OK(), "%s", r)
	r = cgctl("logs", fmt.Sprint(id), "../access.log", "-")
	h.assert(t, "#84: a name that is not a log is a usage error", !r.OK() && r.Code == 2, "%s", r)
	r = cgctl("logs", fmt.Sprint(id))
	h.assert(t, "#84: cgctl lists the logs", r.OK() && strings.Contains(r.Out, "access.log.2.gz"), "%s", r)
}

// getRaw is a GET whose answer's headers are wanted too (a download's
// length, type and name); a transport error is code 0.
func (a *apiClient) getRaw(path string) (int, http.Header, []byte) {
	a.h.step("GET " + path)
	req, err := http.NewRequest("GET", a.h.Panel+path, nil)
	if err != nil {
		return 0, nil, nil
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		a.h.root.Logf("API GET %s: %v", path, err)
		return 0, nil, nil
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}
