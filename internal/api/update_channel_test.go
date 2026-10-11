package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/version"
)

// The release check follows the update_channel setting (R-6.3): stable is
// offered only vX.Y.Z, beta also the pre-releases, and switching the
// channel drops the cached answer.
func TestUpdateCheckFollowsTheChannel(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	files := map[string][]byte{}
	var tags []string
	publish := func(tag string) {
		s := sha256.Sum256([]byte(tag))
		body := "version " + tag + "\n" + hex.EncodeToString(s[:]) + "  cgctl\n"
		files["/releases/download/"+tag+"/SHA256SUMS"] = []byte(body)
		files["/releases/download/"+tag+"/SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(body))))
		tags = append(tags, tag)
	}
	publish("v9.0.0")
	publish("v9.1.0-beta.1")
	publish("v8.0.0")
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases.atom" {
			var b strings.Builder
			b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom">`)
			for _, tag := range tags {
				fmt.Fprintf(&b, `<entry><id>x/%s</id><link href="https://example.com/o/r/releases/tag/%s"/></entry>`, tag, tag)
			}
			b.WriteString(`</feed>`)
			w.Write([]byte(b.String()))
			return
		}
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer feed.Close()
	oldURL, oldKey, oldVer := version.UpdateURL, version.UpdatePublicKey, version.Version
	version.UpdateURL, version.UpdatePublicKey, version.Version = feed.URL, base64.StdEncoding.EncodeToString(pub), "v8.0.0"
	defer func() { version.UpdateURL, version.UpdatePublicKey, version.Version = oldURL, oldKey, oldVer }()

	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	status := func() map[string]any {
		t.Helper()
		resp, body := c.do("GET", "/api/system/update?refresh=1", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update status = %d %s", resp.StatusCode, body)
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if out := status(); out["channel"] != "stable" || out["latest"] != "v9.0.0" || out["update_available"] != true {
		t.Fatalf("stable: %v", out)
	}
	resp, body := c.do("GET", "/api/settings", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"update_channel":"stable"`) {
		t.Fatalf("settings = %d %s", resp.StatusCode, body)
	}
	if resp, body := c.do("PUT", "/api/settings", map[string]string{"update_channel": "beta"}); resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), `"update_channel":"beta"`) {
		t.Fatalf("choosing beta = %d %s", resp.StatusCode, body)
	}
	if out := status(); out["channel"] != "beta" || out["latest"] != "v9.1.0-beta.1" {
		t.Fatalf("beta: %v", out)
	}
	resp, body = c.do("PUT", "/api/settings", map[string]string{"update_channel": "nightly"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "update_channel: must be stable or beta") {
		t.Fatalf("an unknown channel = %d %s", resp.StatusCode, body)
	}
	if ps, _ := s.Store.PanelSettings(); ps.UpdateChannel != "beta" {
		t.Fatalf("a refused channel changed the setting to %q", ps.UpdateChannel)
	}
}
