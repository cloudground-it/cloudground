//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"
)

// filesEditor: the file manager's editor opens only text. A binary file
// read through the API comes back flagged with no content, and text sent
// for it is refused without touching it. Run on a provisioned site (id,
// root, system user).
func (h *harness) filesEditor(t *testing.T, id int64, root, user string) {
	text, bin := root+"/current/cg-e2e-editor.txt", root+"/current/cg-e2e-editor.png"
	defer h.remove(text, bin)
	const binData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01"
	h.mustWriteAs(t, text, "caffè\n", user)
	h.mustWriteAs(t, bin, binData, user)

	type readResult struct {
		Content  *string `json:"content"`
		Binary   *bool   `json:"binary"`
		TooLarge bool    `json:"too_large"`
		Size     int64   `json:"size"`
		Encoding string  `json:"encoding"`
	}
	read := func(rel string) (int, readResult) {
		c, b := h.api.get(fmt.Sprintf("/api/sites/%d/files/read?path=%s", id, rel))
		var r readResult
		_ = json.Unmarshal(b, &r)
		return c, r
	}
	// Control: a text file opens, with its content, so the binary verdict
	// below comes from the detection and not from a read that failed.
	c, r := read("current/cg-e2e-editor.txt")
	h.assert(t, "control: a text file opens in the editor",
		c == 200 && r.Binary != nil && !*r.Binary && r.Content != nil && *r.Content == "caffè\n" && r.Encoding == "utf-8",
		"text file read: %d %+v", c, r)

	c, r = read("current/cg-e2e-editor.png")
	h.assert(t, "a binary file is reported binary, without content",
		c == 200 && r.Binary != nil && *r.Binary && (r.Content == nil || *r.Content == "") && r.Size == int64(len(binData)),
		"binary file read: %d %+v", c, r)

	wc, wb := h.api.post(fmt.Sprintf("/api/sites/%d/files/write", id), `{"path":"current/cg-e2e-editor.png","content":"overwritten"}`)
	h.assert(t, "text written over a binary file is refused (409)", wc == 409, "write over a binary file: %d %s", wc, head(wb, 200))
	got, _ := os.ReadFile(bin)
	h.assert(t, "the refused write left the binary file intact", string(got) == binData, "binary file changed to %q", head(got, 40))

	// Control for the refusal: the same request for the text file saves.
	wc, wb = h.api.post(fmt.Sprintf("/api/sites/%d/files/write", id), `{"path":"current/cg-e2e-editor.txt","content":"saved\n"}`)
	got, _ = os.ReadFile(text)
	h.assert(t, "control: text written over a text file is saved", wc == 200 && string(got) == "saved\n", "write over a text file: %d %s, file %q", wc, head(wb, 200), got)

	// R-3.1: a folder name already taken is a conflict the browser can show,
	// not a failed agent (the old agent answered 502).
	dir := root + "/current/cg-e2e-dir"
	defer h.remove(dir)
	mkdir := func() (int, []byte) {
		return h.api.post(fmt.Sprintf("/api/sites/%d/files/manage", id), `{"operation":"mkdir","path":"current/cg-e2e-dir"}`)
	}
	mc, mb := mkdir()
	st, err := os.Stat(dir)
	h.assert(t, "control: the file manager makes a folder", mc == 200 && err == nil && st.IsDir(), "mkdir: %d %s (%v)", mc, head(mb, 200), err)
	mc, mb = mkdir()
	h.assert(t, "R-3.1: making a folder that exists answers 409", mc == 409, "mkdir of an existing folder: %d %s", mc, head(mb, 200))
}

// filesUploadThroughNginx: the browser reaches the panel through nginx on
// :8443, not through the API's socket. nginx's 1 MB default body limit
// answered 413 to every larger upload before the panel saw it (measured),
// so the size the panel advertises must actually get through.
func (h *harness) filesUploadThroughNginx(t *testing.T, id int64, root string) {
	const size = 2 << 20
	base := "https://" + h.IP + ":8443"
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the panel's self-signed IP certificate
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:8443")
			},
		},
	}
	post := func(path, ctype string, body io.Reader) (int, []byte) {
		r, err := http.NewRequest("POST", base+path, body)
		if err != nil {
			return 0, []byte(err.Error())
		}
		r.Header.Set("Content-Type", ctype)
		r.Header.Set("Origin", base)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(r)
		if err != nil {
			return 0, []byte(err.Error())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, b
	}
	login, _ := json.Marshal(map[string]string{"email": h.Email, "password": h.PW})
	lc, lb := post("/api/login", "application/json", bytes.NewReader(login))
	h.assert(t, "control: the admin signs in through nginx on :8443", lc == 200, "login through :8443: %d %s", lc, head(lb, 200))

	dest := "current/cg-e2e-upload-2mb.bin"
	defer h.remove(root + "/" + dest)
	uc, ub := post(fmt.Sprintf("/api/sites/%d/files/upload?path=%s", id, dest), "application/octet-stream",
		bytes.NewReader(bytes.Repeat([]byte{'x'}, size)))
	st, err := os.Stat(root + "/" + dest)
	h.assert(t, "a 2 MB upload through nginx reaches the panel and is stored",
		uc == 201 && err == nil && st.Size() == size,
		"upload through :8443: %d %s (stored: %v)", uc, head(ub, 200), err)
}
