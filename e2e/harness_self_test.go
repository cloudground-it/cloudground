//go:build e2e

package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHarnessAPIClient checks the client against a fake panel on a Unix
// socket: the Secure session cookie is kept and sent, a created site is
// parsed and tasks and deletions are polled to their end. It needs no
// server, so it can run anywhere the e2e tag builds.
func TestHarnessAPIClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	var polls, deleted atomic.Int32
	mux := http.NewServeMux()
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("__Host-cg_session")
		return err == nil && c.Value == "tok"
	}
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Content-Type") != "application/json" || body["password"] != "pw" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-cg_session", Value: "tok", Path: "/", Secure: true, HttpOnly: true})
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /api/sites", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"site":{"id":42,"status":"provisioning"},"task":{"id":7}}`))
	})
	mux.HandleFunc("GET /api/sites/42", func(w http.ResponseWriter, r *http.Request) {
		if deleted.Load() > 0 {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"id":42,"status":"active","system_user":"cg_x","config":{"object_cache":true,"database":{"name":"db1"}}}`))
	})
	mux.HandleFunc("DELETE /api/sites/42", func(w http.ResponseWriter, r *http.Request) {
		deleted.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /api/tasks/7", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) < 2 {
			w.Write([]byte(`{"status":"running"}`))
			return
		}
		w.Write([]byte(`{"status":"succeeded"}`))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	// Closed after the harness's own cleanups, which still talk to it.
	t.Cleanup(func() { srv.Close() })

	h := &harness{root: t, env: env{Panel: "http://localhost", Sock: sock}, canary: newCanary(t)}
	h.api = newAPIClient(h, "a@b.c", "pw")
	if !h.api.login() {
		t.Fatal("login failed")
	}
	id := h.api.createSite(`{"domain":"x","type":"static"}`)
	if id != 42 {
		t.Fatalf("site id = %d", id)
	}
	s := h.api.site(id)
	if s.SystemUser != "cg_x" || !s.Config.ObjectCache || s.Config.Database.Name != "db1" {
		t.Errorf("site = %+v", s)
	}
	if st := h.api.waitActive(id, 10*time.Second); st != "active" {
		t.Errorf("waitActive = %s", st)
	}
	if st := h.api.waitTask(7, 30*time.Second); st != "succeeded" {
		t.Errorf("waitTask = %s", st)
	}
	if st := h.api.waitTask(0, time.Hour); st != "timeout" {
		t.Errorf("waitTask(0) = %s", st)
	}
	if c := h.api.deleteSite(id); c != "202" {
		t.Errorf("delete = %s", c)
	}
	if g := h.api.teardownGone(id); g != "yes" {
		t.Errorf("teardownGone = %s", g)
	}
	c, b := h.api.do("POST", "/api/nowhere", "x", "Content-Type: text/plain")
	if c != 404 || !strings.Contains(string(b), "404") {
		t.Errorf("404 path: %d %s", c, b)
	}
}
